package main

import (
	"log"
	"os"

	loggingproxy "github.com/mrexodia/logging-proxy"
	"gopkg.in/yaml.v3"
)

func main() {
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatal("Error loading config:", err)
	}

	server := loggingproxy.NewProxyServer(config)
	server.Start()
}

func loadConfig(filename string) (*loggingproxy.Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config loggingproxy.Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}
