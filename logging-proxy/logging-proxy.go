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

// Route defines a proxy route configuration.
// Pattern uses Go's http.ServeMux pattern syntax (Go 1.22+):
//   - "/api/" matches "/api/" and everything under it (like "/api/v1/chat")
//   - "/exact" matches only "/exact"
//   - "/" is a catch-all that matches everything
//   - Wildcards like "{id}" and "{path...}" are supported
type Route struct {
	Pattern     string `yaml:"pattern"`
	Destination string `yaml:"destination"`
}

type Config struct {
	Server struct {
		Port     int    `yaml:"port"`
		Host     string `yaml:"host"`
		NotFound string `yaml:"not_found"`
	} `yaml:"server"`
	Logging struct {
		Console bool   `yaml:"console"`
		File    bool   `yaml:"file"`
		LogDir  string `yaml:"log_dir"`
	} `yaml:"logging"`
	Routes map[string]Route `yaml:"routes"`
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

	proxy := loggingproxy.NewProxyServer(config.Server.NotFound)

	// Configure logger
	var logger loggingproxy.Logger = &loggingproxy.NoOpLogger{}
	if config.Logging.File {
		logDir := config.Logging.LogDir
		if logDir == "" {
			logDir = "logs"
		}
		if fileLogger, err := loggingproxy.NewFileLogger(logDir); err != nil {
			log.Printf("Failed to create file logger: %v, using no-op logger", err)
		} else {
			logger = fileLogger
		}
		fmt.Printf("Logging: file logging to %s\n", logDir)
	}

	// Add routes
	hasCatchAll := false
	for _, route := range config.Routes {
		fmt.Printf("[route] %s -> %s\n", route.Pattern, route.Destination)
		if !strings.HasSuffix(route.Pattern, "/") {
			fmt.Printf("  (warning) Pattern %q has no trailing '/'; will not match subpaths\n", route.Pattern)
		}
		if err := proxy.AddRoute(route.Pattern, route.Destination, logger); err != nil {
			log.Fatalf("Failed to add route %s: %v", route.Pattern, err)
		}
		if route.Pattern == "/" {
			hasCatchAll = true
		}
	}

	// Set up catch-all handler if no "/" route was configured
	if !hasCatchAll && config.Server.NotFound != "" {
		notFoundUrl := fmt.Sprintf("http://%s:%d%s", config.Server.Host, config.Server.Port, config.Server.NotFound)
		fmt.Printf("Registering catch-all handler: %s\n", notFoundUrl)
		if err := proxy.AddRoute("/", notFoundUrl, logger); err != nil {
			log.Fatalf("Failed to add catch-all route: %v", err)
		}
	}

	addr := fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port)
	fmt.Printf("Proxy server starting on %s\n", addr)

	// Start proxy server
	server := http.Server{
		Addr:                         addr,
		Handler:                      proxy,
		DisableGeneralOptionsHandler: true,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal("Server failed:", err)
	}
}

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}
