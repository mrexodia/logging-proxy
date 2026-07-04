package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	loggingproxy "github.com/mrexodia/logging-proxy"
	"golang.org/x/net/http/httpproxy"
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

type ProxyAuthConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type ProxyConfig struct {
	Host    string           `yaml:"host"`
	Port    int              `yaml:"port"`
	Verbose bool             `yaml:"verbose"`
	Auth    *ProxyAuthConfig `yaml:"auth"`
	MITM    struct {
		Enabled                   bool     `yaml:"enabled"`
		CertsDir                  string   `yaml:"certs_dir"`
		Organization              string   `yaml:"organization"`
		Hostname                  string   `yaml:"hostname"`
		IncludeHosts              []string `yaml:"include_hosts"`
		ExcludeHosts              []string `yaml:"exclude_hosts"`
		LoggingExcludeURLPrefixes []string `yaml:"logging_exclude_url_prefixes"`
	} `yaml:"mitm"`
}

type HTTPClientConfig struct {
	ProxyURL             string `yaml:"proxy_url"`
	ProxyFromEnvironment *bool  `yaml:"proxy_from_environment"`
}

type ServerConfig struct {
	Port     int    `yaml:"port"`
	Host     string `yaml:"host"`
	NotFound string `yaml:"not_found"`
}

type Config struct {
	Server  *ServerConfig `yaml:"server"`
	Logging struct {
		Enabled bool   `yaml:"enabled"`
		Console bool   `yaml:"console"`
		LogDir  string `yaml:"log_dir"`
	} `yaml:"logging"`
	HTTPClient HTTPClientConfig `yaml:"http_client"`
	// proxy is optional. If present, a forward proxy listener is started.
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
	proxyEndpoints, proxyLogMessage, err := describeHTTPClientProxyConfig(clientProxyConfig)
	if err != nil {
		log.Fatal(err)
	}
	if err := validateHTTPClientProxyEndpoints(proxyEndpoints, configuredListenerAddresses(config)); err != nil {
		log.Fatal(err)
	}
	log.Print(proxyLogMessage)

	servers := []namedServer{}
	if config.Server != nil {
		reverseHandler, err := buildReverseProxy(config, logger, clientProxyConfig)
		if err != nil {
			log.Fatal(err)
		}
		servers = append(servers, namedServer{
			name: "reverse",
			server: &http.Server{
				Addr:                         fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port),
				Handler:                      reverseHandler,
				DisableGeneralOptionsHandler: true,
			},
		})
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

type httpClientProxyEndpoint struct {
	label string
	url   *url.URL
}

type listenerAddress struct {
	name string
	host string
	port int
}

func describeHTTPClientProxyConfig(config loggingproxy.HTTPClientProxyConfig) ([]httpClientProxyEndpoint, string, error) {
	if strings.TrimSpace(config.ProxyURL) != "" {
		proxyURL, err := loggingproxy.ParseHTTPClientProxyURL(config.ProxyURL)
		if err != nil {
			return nil, "", err
		}
		return []httpClientProxyEndpoint{{label: "http_client.proxy_url", url: proxyURL}},
			fmt.Sprintf("HTTP client proxy: %s (from http_client.proxy_url)", proxyURL.Redacted()), nil
	}

	if config.ProxyFromEnvironment != nil && !*config.ProxyFromEnvironment {
		return nil, "HTTP client proxy: disabled", nil
	}

	envConfig := httpproxy.FromEnvironment()
	endpoints := []httpClientProxyEndpoint{}
	parts := []string{}
	if strings.TrimSpace(envConfig.HTTPProxy) != "" {
		proxyURL, err := loggingproxy.ParseHTTPClientProxyURL(envConfig.HTTPProxy)
		if err != nil {
			return nil, "", fmt.Errorf("invalid HTTP_PROXY: %w", err)
		}
		endpoints = append(endpoints, httpClientProxyEndpoint{label: "HTTP_PROXY", url: proxyURL})
		parts = append(parts, fmt.Sprintf("HTTP_PROXY=%s", proxyURL.Redacted()))
	}
	if strings.TrimSpace(envConfig.HTTPSProxy) != "" {
		proxyURL, err := loggingproxy.ParseHTTPClientProxyURL(envConfig.HTTPSProxy)
		if err != nil {
			return nil, "", fmt.Errorf("invalid HTTPS_PROXY: %w", err)
		}
		endpoints = append(endpoints, httpClientProxyEndpoint{label: "HTTPS_PROXY", url: proxyURL})
		parts = append(parts, fmt.Sprintf("HTTPS_PROXY=%s", proxyURL.Redacted()))
	}
	if strings.TrimSpace(envConfig.NoProxy) != "" {
		parts = append(parts, fmt.Sprintf("NO_PROXY=%q", envConfig.NoProxy))
	}
	if len(endpoints) == 0 {
		return nil, "HTTP client proxy: none configured", nil
	}
	return endpoints, "HTTP client proxy: " + strings.Join(parts, ", "), nil
}

func configuredListenerAddresses(config *Config) []listenerAddress {
	listeners := []listenerAddress{}
	if config.Server != nil {
		listeners = append(listeners, listenerAddress{name: "reverse", host: config.Server.Host, port: config.Server.Port})
	}
	if config.Proxy != nil {
		listeners = append(listeners, listenerAddress{name: "forward", host: config.Proxy.Host, port: config.Proxy.Port})
	}
	return listeners
}

func validateHTTPClientProxyEndpoints(endpoints []httpClientProxyEndpoint, listeners []listenerAddress) error {
	for _, endpoint := range endpoints {
		for _, listener := range listeners {
			pointsToSelf, err := proxyEndpointPointsToListener(endpoint.url, listener)
			if err != nil {
				return fmt.Errorf("failed to resolve HTTP client proxy %s=%s against %s listener %s:%d: %w", endpoint.label, endpoint.url.Redacted(), listener.name, listener.host, listener.port, err)
			}
			if pointsToSelf {
				return fmt.Errorf("HTTP client proxy %s=%s points to this process's %s listener at %s:%d", endpoint.label, endpoint.url.Redacted(), listener.name, listener.host, listener.port)
			}
		}
	}
	return nil
}

func proxyEndpointPointsToListener(proxyURL *url.URL, listener listenerAddress) (bool, error) {
	if proxyURL == nil {
		return false, nil
	}
	proxyPort, err := proxyURLPort(proxyURL)
	if err != nil {
		return false, err
	}
	if proxyPort != listener.port {
		return false, nil
	}

	proxyHost := proxyURL.Hostname()
	listenerHost := listener.host
	if isWildcardHost(proxyHost) {
		return true, nil
	}
	if strings.EqualFold(normalizeAddressHost(proxyHost), normalizeAddressHost(listenerHost)) {
		return true, nil
	}
	if isLoopbackHost(proxyHost) && isLoopbackHost(listenerHost) {
		return true, nil
	}
	if isWildcardHost(listenerHost) {
		return hostResolvesToLocal(proxyHost)
	}

	proxyIPs, err := resolveHostIPs(proxyHost)
	if err != nil {
		return false, err
	}
	listenerIPs, err := resolveHostIPs(listenerHost)
	if err != nil {
		return false, err
	}
	return ipSetsIntersect(proxyIPs, listenerIPs), nil
}

func proxyURLPort(proxyURL *url.URL) (int, error) {
	if port := proxyURL.Port(); port != "" {
		parsedPort, err := strconv.Atoi(port)
		if err != nil {
			return 0, fmt.Errorf("invalid proxy port %q", port)
		}
		return parsedPort, nil
	}

	switch strings.ToLower(proxyURL.Scheme) {
	case "http":
		return 80, nil
	case "https":
		return 443, nil
	case "socks5", "socks5h":
		return 1080, nil
	default:
		return 0, fmt.Errorf("unsupported proxy scheme %q", proxyURL.Scheme)
	}
}

func hostResolvesToLocal(host string) (bool, error) {
	hostIPs, err := resolveHostIPs(host)
	if err != nil {
		return false, err
	}
	localIPs, err := localHostIPs()
	if err != nil {
		return false, err
	}
	return ipSetsIntersect(hostIPs, localIPs), nil
}

func localHostIPs() ([]net.IP, error) {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			switch value := addr.(type) {
			case *net.IPNet:
				ips = append(ips, value.IP)
			case *net.IPAddr:
				ips = append(ips, value.IP)
			}
		}
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		if hostnameIPs, err := net.LookupIP(hostname); err == nil {
			ips = append(ips, hostnameIPs...)
		}
	}
	return ips, nil
}

func resolveHostIPs(host string) ([]net.IP, error) {
	host = normalizeAddressHost(host)
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("host %q resolved to no addresses", host)
	}
	return ips, nil
}

func ipSetsIntersect(a, b []net.IP) bool {
	for _, left := range a {
		for _, right := range b {
			if left != nil && right != nil && left.Equal(right) {
				return true
			}
		}
	}
	return false
}

func isWildcardHost(host string) bool {
	host = normalizeAddressHost(host)
	return host == "" || host == "0.0.0.0" || host == "::"
}

func isLoopbackHost(host string) bool {
	host = normalizeAddressHost(host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func normalizeAddressHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
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
		Logger:                    globalLogger,
		MITM:                      config.MITM.Enabled,
		MITMIncludeHosts:          config.MITM.IncludeHosts,
		MITMExcludeHosts:          config.MITM.ExcludeHosts,
		LoggingExcludeURLPrefixes: config.MITM.LoggingExcludeURLPrefixes,
		ClientProxy:               clientProxyConfig,
		Verbose:                   config.Verbose,
	}

	if config.Auth != nil {
		if config.Auth.Username == "" || config.Auth.Password == "" {
			return nil, fmt.Errorf("proxy.auth requires both username and password")
		}
		options.Auth = loggingproxy.HTTPProxyAuthConfig{
			Username: config.Auth.Username,
			Password: config.Auth.Password,
		}
		log.Printf("Forward proxy authentication enabled for user %q", config.Auth.Username)
	}

	if config.MITM.Enabled {
		crlHost, err := crlHostname(config.MITM.Hostname, config.Host, config.Port)
		if err != nil {
			return nil, err
		}
		ca, err := loggingproxy.NewMITMCA(loggingproxy.MITMCAConfig{
			CertsDir:     config.MITM.CertsDir,
			Organization: config.MITM.Organization,
			CRLHost:      crlHost,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize MITM CA: %w", err)
		}
		options.MITMCA = ca
		if len(config.MITM.IncludeHosts) > 0 {
			log.Printf("MITM included hosts: %s", strings.Join(config.MITM.IncludeHosts, ", "))
		}
		if len(config.MITM.ExcludeHosts) > 0 {
			log.Printf("MITM excluded hosts: %s", strings.Join(config.MITM.ExcludeHosts, ", "))
		}
		if len(config.MITM.LoggingExcludeURLPrefixes) > 0 {
			log.Printf("MITM logging excluded URL prefixes: %s", strings.Join(config.MITM.LoggingExcludeURLPrefixes, ", "))
		}
	}

	proxy, err := loggingproxy.NewHTTPProxyServer(options)
	if err != nil {
		return nil, err
	}
	return proxy, nil
}

func crlHostname(configuredHostname, listenHost string, port int) (string, error) {
	host := strings.TrimSpace(configuredHostname)
	if host == "" {
		host = strings.TrimSpace(listenHost)
	}
	if strings.Contains(host, "://") {
		return "", fmt.Errorf("proxy.mitm.hostname must be a hostname or host:port, not a URL: %q", host)
	}
	if isWildcardHost(host) {
		return "", fmt.Errorf("proxy.mitm.hostname is required when proxy.host is %q", listenHost)
	}
	if splitHost, splitPort, err := net.SplitHostPort(host); err == nil {
		if splitHost == "" || splitPort == "" || isWildcardHost(splitHost) {
			return "", fmt.Errorf("invalid proxy.mitm.hostname %q", host)
		}
		return host, nil
	}
	host = normalizeAddressHost(host)
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
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

	if config.Server == nil && len(config.Routes) > 0 {
		return nil, fmt.Errorf("routes require a server section")
	}
	if config.Server == nil && config.Proxy == nil {
		return nil, fmt.Errorf("configuration must include at least one of server or proxy")
	}
	if config.Server != nil {
		if config.Server.Host == "" {
			config.Server.Host = "localhost"
		}
		if config.Server.Port == 0 {
			config.Server.Port = 5601
		}
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
