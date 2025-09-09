package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Host string `yaml:"host"`
	} `yaml:"server"`
	Logging struct {
		Console   bool   `yaml:"console"`
		ServerURL string `yaml:"server_url"`
		Enabled   bool   `yaml:"enabled"`
	} `yaml:"logging"`
	Routes []Route `yaml:"routes"`
}

type Route struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
	Name        string `yaml:"name"`
}

type ProxyServer struct {
	config       *Config
	routes       map[string]*Route
	loggingClient *LoggingClient
}

type LoggingClient struct {
	serverURL string
	client    *http.Client
	enabled   bool
}

func main() {
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatal("Error loading config:", err)
	}

	server := NewProxyServer(config)
	server.Start()
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

func NewProxyServer(config *Config) *ProxyServer {
	server := &ProxyServer{
		config: config,
		routes: make(map[string]*Route),
		loggingClient: &LoggingClient{
			serverURL: config.Logging.ServerURL,
			client:    &http.Client{Timeout: 30 * time.Second},
			enabled:   config.Logging.Enabled,
		},
	}

	// Initialize routes map
	for i := range config.Routes {
		server.routes[config.Routes[i].Source] = &config.Routes[i]
	}

	return server
}

// findMatchingRoute finds the best matching route for the given path
func (s *ProxyServer) findMatchingRoute(path string) *Route {
	var bestMatch *Route
	var bestLength int

	for sourcePath, route := range s.routes {
		trimmedSource := strings.TrimSuffix(sourcePath, "/")
		if strings.HasPrefix(path, trimmedSource) && len(trimmedSource) > bestLength {
			bestMatch = route
			bestLength = len(trimmedSource)
		}
	}

	return bestMatch
}

// transformPath converts the source path to the destination path
func transformPath(originalPath, sourcePath, destinationURL string) (string, error) {
	sourceTrimmed := strings.TrimSuffix(sourcePath, "/")
	remainingPath := strings.TrimPrefix(originalPath, sourceTrimmed)
	
	destURL, err := url.Parse(destinationURL)
	if err != nil {
		return "", err
	}
	
	destPath := strings.TrimSuffix(destURL.Path, "/")
	if destPath == "" || destPath == "/" {
		return remainingPath, nil
	}
	
	return destPath + remainingPath, nil
}

func (s *ProxyServer) Start() {
	// Single handler for all requests
	http.HandleFunc("/", s.handleRequest)
	
	// Display configured routes
	for sourcePath, route := range s.routes {
		fmt.Printf("Route: %s -> %s (%s)\n", 
			sourcePath, route.Destination, route.Name)
	}

	addr := fmt.Sprintf("%s:%d", s.config.Server.Host, s.config.Server.Port)
	fmt.Printf("Proxy server starting on %s\n", addr)
	if s.config.Logging.Enabled {
		fmt.Printf("Logging server: %s\n", s.config.Logging.ServerURL)
	}
	
	log.Fatal(http.ListenAndServe(addr, nil))
}

// handleRequest processes incoming requests with streaming duplex architecture
func (s *ProxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	
	// Find matching route
	route := s.findMatchingRoute(r.URL.Path)
	if route == nil {
		http.NotFound(w, r)
		return
	}

	// Generate unique request ID
	requestID := uuid.New().String()
	
	if s.config.Logging.Console {
		fmt.Printf("%s [%s] %s %s -> %s [%s]\n", 
			start.Format("2006-01-02 15:04:05"),
			requestID[:8], 
			r.Method, 
			r.URL.Path, 
			route.Destination, 
			route.Name)
	}

	// Transform the path
	targetPath, err := transformPath(r.URL.Path, route.Source, route.Destination)
	if err != nil {
		http.Error(w, "Invalid destination URL", http.StatusInternalServerError)
		return
	}

	// Parse target URL
	targetURL, err := url.Parse(route.Destination)
	if err != nil {
		http.Error(w, "Invalid destination URL", http.StatusInternalServerError)
		return
	}
	
	// Build target URL
	targetURL.Path = targetPath
	targetURL.RawQuery = r.URL.RawQuery

	// Create the duplex streaming proxy request
	err = s.proxyWithDuplex(w, r, targetURL, requestID, route.Name)
	if err != nil {
		log.Printf("Proxy error for %s: %v", requestID, err)
		http.Error(w, "Proxy error", http.StatusBadGateway)
	}
}

// proxyWithDuplex handles the duplex streaming proxy logic
func (s *ProxyServer) proxyWithDuplex(w http.ResponseWriter, originalReq *http.Request, targetURL *url.URL, requestID, routeName string) error {
	// Create the request to the target server
	proxyReq, err := http.NewRequest(originalReq.Method, targetURL.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create proxy request: %w", err)
	}

	// Copy headers from original request
	for key, values := range originalReq.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}

	// Handle request body with duplex streaming
	if originalReq.Body != nil && s.loggingClient.enabled {
		// Create a pipe for the logging stream
		logPipeReader, logPipeWriter := io.Pipe()
		
		// Start async logging of request
		go s.streamToLoggingServer(logPipeReader, requestID, "request", originalReq.Header)
		
		// Use TeeReader to duplicate the stream
		teeReader := io.TeeReader(originalReq.Body, logPipeWriter)
		proxyReq.Body = &requestBodyCloser{
			Reader:     teeReader,
			original:   originalReq.Body,
			pipeWriter: logPipeWriter,
		}
	} else {
		// Simple case - just forward the request body
		proxyReq.Body = originalReq.Body
	}

	// Make the request to target server
	client := &http.Client{}
	resp, err := client.Do(proxyReq)
	if err != nil {
		return fmt.Errorf("proxy request failed: %w", err)
	}
	defer resp.Body.Close()

	// Copy response headers to client
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Handle response body with duplex streaming
	if s.loggingClient.enabled {
		// Create a pipe for the logging stream
		logPipeReader, logPipeWriter := io.Pipe()
		
		// Start async logging of response
		go s.streamToLoggingServer(logPipeReader, requestID, "response", resp.Header)
		
		// Use MultiWriter to duplicate the stream to both client and logging
		multiWriter := io.MultiWriter(w, logPipeWriter)
		
		// Stream response to both destinations
		_, err = io.Copy(multiWriter, resp.Body)
		logPipeWriter.Close() // Signal end of stream to logger
	} else {
		// Simple case - just stream to client
		_, err = io.Copy(w, resp.Body)
	}
	
	return err
}

// requestBodyCloser wraps a TeeReader and handles proper cleanup for request bodies
type requestBodyCloser struct {
	io.Reader
	original   io.ReadCloser
	pipeWriter *io.PipeWriter
}

func (r *requestBodyCloser) Close() error {
	r.pipeWriter.Close()
	return r.original.Close()
}

// streamToLoggingServer sends the raw HTTP data to the logging server
func (s *ProxyServer) streamToLoggingServer(reader io.Reader, requestID, streamType string, headers http.Header) {
	if !s.loggingClient.enabled {
		return
	}

	// Create a pipe for streaming the data
	pipeReader, pipeWriter := io.Pipe()
	
	// Start a goroutine to write headers and body to the pipe
	go func() {
		defer pipeWriter.Close()
		
		// First, write the headers
		for key, values := range headers {
			for _, value := range values {
				fmt.Fprintf(pipeWriter, "%s: %s\r\n", key, value)
			}
		}
		fmt.Fprintf(pipeWriter, "\r\n") // Empty line separating headers from body
		
		// Stream the body
		_, err := io.Copy(pipeWriter, reader)
		if err != nil {
			log.Printf("Error streaming %s data for %s: %v", streamType, requestID, err)
		}
	}()

	// Send to logging server using the pipe
	url := fmt.Sprintf("%s/%s/%s", s.loggingClient.serverURL, requestID, streamType)
	req, err := http.NewRequest("PUT", url, pipeReader)
	if err != nil {
		log.Printf("Error creating logging request for %s %s: %v", requestID, streamType, err)
		return
	}

	req.Header.Set("Content-Type", "application/octet-stream")
	
	resp, err := s.loggingClient.client.Do(req)
	if err != nil {
		log.Printf("Error sending %s log for %s: %v", streamType, requestID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		log.Printf("Logging server returned %d for %s %s", resp.StatusCode, requestID, streamType)
	}
}
