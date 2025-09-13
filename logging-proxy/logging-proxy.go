package main

import (
	"fmt"
	"log"
	"os"

	loggingproxy "github.com/mrexodia/logging-proxy"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Host string `yaml:"host"`
	} `yaml:"server"`
	Logging struct {
		Console bool   `yaml:"console"`
		File    bool   `yaml:"file"`
		LogDir  string `yaml:"log_dir"`
	} `yaml:"logging"`
	Routes map[string]loggingproxy.Route `yaml:"routes"`
}

func main() {
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatal("Error loading config:", err)
	}

	server := loggingproxy.NewProxyServer()

	// Configure logger if needed
	if config.Logging.File {
		logDir := config.Logging.LogDir
		if logDir == "" {
			logDir = "logs"
		}
		if fileLogger, err := loggingproxy.NewFileLogger(logDir); err != nil {
			log.Printf("Failed to create file logger: %v, using no-op logger", err)
		} else {
			server.SetLogger(fileLogger)
		}
		fmt.Printf("Logging: file logging to %s\n", logDir)
	}

	// Add routes
	hasCatchAll := false
	for _, route := range config.Routes {
		fmt.Printf("Route: %s -> %s\n", route.Pattern, route.Destination)
		server.AddRoute(route.Pattern, route.Destination)
		if route.Pattern == "/" {
			hasCatchAll = true
		}
	}

	// Set up catch-all handler if no "/" route was configured
	if !hasCatchAll {
		server.SetCatchAllHandler()
	}

	addr := fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port)
	server.Start(addr)
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
