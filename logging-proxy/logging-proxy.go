package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	loggingproxy "github.com/mrexodia/logging-proxy"
	"gopkg.in/yaml.v3"
)

// Route defines a reverse proxy route configuration.
// Pattern uses Go's http.ServeMux pattern syntax (Go 1.22+):
//   - "/api/" matches "/api/" and everything under it (like "/api/v1/chat")
//   - "/exact" matches only "/exact"
//   - "/" is a catch-all that matches everything
//   - Go ServeMux supports wildcards, but this proxy currently rejects named
//     wildcards like "{id}" and "{path...}" in configured patterns
//   - The special end-anchor pattern "{$}" is still allowed
//
// Logging defaults to logging.enabled unless explicitly overridden per-route.
type Route struct {
	Pattern     string `yaml:"pattern"`
	Destination string `yaml:"destination"`
	Logging     *bool  `yaml:"logging"`
}

type ProxyConfig struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	Verbose bool   `yaml:"verbose"`
	MITM    struct {
		Enabled      bool     `yaml:"enabled"`
		CertFile     string   `yaml:"cert_file"`
		KeyFile      string   `yaml:"key_file"`
		CommonName   string   `yaml:"common_name"`
		Organization string   `yaml:"organization"`
		ExcludeHosts []string `yaml:"exclude_hosts"`
	} `yaml:"mitm"`
}

type HTTPClientConfig struct {
	ProxyURL             string `yaml:"proxy_url"`
	ProxyFromEnvironment *bool  `yaml:"proxy_from_environment"`
}

type Config struct {
	Server struct {
		Port     int    `yaml:"port"`
		Host     string `yaml:"host"`
		NotFound string `yaml:"not_found"`
	} `yaml:"server"`
	Logging struct {
		Enabled bool   `yaml:"enabled"`
		Console bool   `yaml:"console"`
		LogDir  string `yaml:"log_dir"`
	} `yaml:"logging"`
	HTTPClient HTTPClientConfig `yaml:"http_client"`
	// proxy is optional. If present, a forward proxy listener is started in addition
	// to the reverse proxy listener configured under server.
	Proxy  *ProxyConfig     `yaml:"proxy"`
	Routes map[string]Route `yaml:"routes"`
}

type namedServer struct {
	name   string
	server *http.Server
}

func main() {
	// Allow passing the config file as the first argument
	configFile := "config.yaml"
	if len(os.Args) > 1 {
		configFile = os.Args[1]
	}

	config, err := loadConfig(configFile)
	if err != nil {
		log.Fatal("Error loading config:", err)
	}

	logger, err := buildGlobalLogger(config)
	if err != nil {
		log.Fatal(err)
	}

	clientProxyConfig := buildHTTPClientProxyConfig(config)
	logHTTPClientProxyConfig(clientProxyConfig)

	reverseHandler, err := buildReverseProxy(config, logger, clientProxyConfig)
	if err != nil {
		log.Fatal(err)
	}

	servers := []namedServer{
		{
			name: "reverse",
			server: &http.Server{
				Addr:                         fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port),
				Handler:                      reverseHandler,
				DisableGeneralOptionsHandler: true,
			},
		},
	}

	if config.Proxy != nil {
		forwardHandler, err := buildForwardProxy(config.Proxy, logger, clientProxyConfig)
		if err != nil {
			log.Fatal(err)
		}
		servers = append(servers, namedServer{
			name: "forward",
			server: &http.Server{
				Addr:                         fmt.Sprintf("%s:%d", config.Proxy.Host, config.Proxy.Port),
				Handler:                      forwardHandler,
				DisableGeneralOptionsHandler: true,
			},
		})
	}

	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		log.Printf("%s proxy starting on %s", srv.name, srv.server.Addr)
		go func(s namedServer) {
			if err := s.server.ListenAndServe(); err != nil {
				errCh <- fmt.Errorf("%s proxy failed: %w", s.name, err)
			}
		}(srv)
	}

	log.Fatal(<-errCh)
}

func buildGlobalLogger(config *Config) (loggingproxy.Logger, error) {
	// Configure logger
	if !config.Logging.Enabled {
		return &loggingproxy.NoOpLogger{}, nil
	}

	logDir := config.Logging.LogDir
	if logDir == "" {
		logDir = "logs"
	}

	fileLogger, err := loggingproxy.NewFileLogger(logDir, config.Logging.Console)
	if err != nil {
		return nil, fmt.Errorf("failed to create file logger: %w", err)
	}
	log.Printf("Logging requests/responses to: %s", logDir)
	return fileLogger, nil
}

func buildHTTPClientProxyConfig(config *Config) loggingproxy.HTTPClientProxyConfig {
	return loggingproxy.HTTPClientProxyConfig{
		ProxyURL:             strings.TrimSpace(config.HTTPClient.ProxyURL),
		ProxyFromEnvironment: config.HTTPClient.ProxyFromEnvironment,
	}
}

func logHTTPClientProxyConfig(config loggingproxy.HTTPClientProxyConfig) {
	if config.ProxyURL != "" {
		log.Printf("HTTP client proxy: %s", config.ProxyURL)
		return
	}
	if config.ProxyFromEnvironment == nil || *config.ProxyFromEnvironment {
		log.Printf("HTTP client proxy: using HTTP_PROXY, HTTPS_PROXY, and NO_PROXY from the environment")
		return
	}
	log.Printf("HTTP client proxy: disabled")
}

func buildReverseProxy(config *Config, globalLogger loggingproxy.Logger, clientProxyConfig loggingproxy.HTTPClientProxyConfig) (http.Handler, error) {
	proxy, err := loggingproxy.NewProxyServerWithHTTPClientProxy(config.Server.NotFound, clientProxyConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to configure reverse proxy HTTP client: %w", err)
	}
	noOpLogger := &loggingproxy.NoOpLogger{}

	hasCatchAll := false
	for _, route := range config.Routes {
		logger := loggingproxy.Logger(noOpLogger)
		loggingEnabled := config.Logging.Enabled
		if route.Logging != nil {
			loggingEnabled = *route.Logging
		}
		if loggingEnabled {
			logger = globalLogger
			log.Printf("[route] %s -> %s (logging enabled)", route.Pattern, route.Destination)
		} else {
			log.Printf("[route] %s -> %s (logging disabled)", route.Pattern, route.Destination)
		}

		if !strings.HasSuffix(route.Pattern, "/") {
			log.Printf("  (warning) Pattern %q has no trailing '/'; will not match subpaths", route.Pattern)
		}

		if err := proxy.AddRoute(route.Pattern, route.Destination, logger); err != nil {
			return nil, fmt.Errorf("failed to add route %s: %w", route.Pattern, err)
		}
		if route.Pattern == "/" {
			hasCatchAll = true
		}
	}

	// Set up catch-all handler if no "/" route was configured
	if !hasCatchAll && config.Server.NotFound != "" {
		notFoundURL := fmt.Sprintf("http://%s:%d%s", config.Server.Host, config.Server.Port, config.Server.NotFound)
		log.Printf("Registering catch-all handler: %s", notFoundURL)
		logger := loggingproxy.Logger(noOpLogger)
		if config.Logging.Enabled {
			logger = globalLogger
		}
		if err := proxy.AddRoute("/", notFoundURL, logger); err != nil {
			return nil, fmt.Errorf("failed to add catch-all route: %w", err)
		}
	}

	return proxy, nil
}

func buildForwardProxy(config *ProxyConfig, globalLogger loggingproxy.Logger, clientProxyConfig loggingproxy.HTTPClientProxyConfig) (http.Handler, error) {
	options := loggingproxy.HTTPProxyOptions{
		Logger:           globalLogger,
		MITM:             config.MITM.Enabled,
		MITMExcludeHosts: config.MITM.ExcludeHosts,
		ClientProxy:      clientProxyConfig,
		Verbose:          config.Verbose,
	}

	if config.MITM.Enabled {
		ca, err := loggingproxy.LoadOrCreateMITMCA(loggingproxy.MITMCAConfig{
			CertFile:     config.MITM.CertFile,
			KeyFile:      config.MITM.KeyFile,
			CommonName:   config.MITM.CommonName,
			Organization: config.MITM.Organization,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize MITM CA: %w", err)
		}
		options.MITMCertificate = ca
		log.Printf("MITM enabled. Trust this CA in Claude Code via NODE_EXTRA_CA_CERTS: %s", defaultString(config.MITM.CertFile, "certs/mitm-ca-cert.pem"))
		if len(config.MITM.ExcludeHosts) > 0 {
			log.Printf("MITM excluded hosts: %s", strings.Join(config.MITM.ExcludeHosts, ", "))
		}
	}

	proxy, err := loggingproxy.NewHTTPProxyServer(options)
	if err != nil {
		return nil, err
	}
	return proxy, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	if config.Server.Host == "" {
		config.Server.Host = "localhost"
	}
	if config.Server.Port == 0 {
		config.Server.Port = 5601
	}
	if config.Proxy != nil {
		if config.Proxy.Host == "" {
			config.Proxy.Host = "localhost"
		}
		if config.Proxy.Port == 0 {
			config.Proxy.Port = 8080
		}
	}

	return &config, nil
}
