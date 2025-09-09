package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Port int    `yaml:"port"`
		Host string `yaml:"host"`
	} `yaml:"server"`
	Logging struct {
		Console     bool   `yaml:"console"`
		File        string `yaml:"file"`
		BinaryFiles bool   `yaml:"binary_files"`
	} `yaml:"logging"`
	Routes []Route `yaml:"routes"`
}

type Route struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
	Name        string `yaml:"name"`
}

type ProxyServer struct {
	config *Config
	routes map[string]*RouteHandler
}

type RouteHandler struct {
	route     Route
	proxy     *httputil.ReverseProxy
	targetURL *url.URL
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
		routes: make(map[string]*RouteHandler),
	}

	// Initialize route handlers
	for _, route := range config.Routes {
		handler, err := NewRouteHandler(route, config)
		if err != nil {
			log.Printf("Error creating handler for route %s: %v", route.Name, err)
			continue
		}
		server.routes[route.Source] = handler
	}

	return server
}

func NewRouteHandler(route Route, config *Config) (*RouteHandler, error) {
	targetURL, err := url.Parse(route.Destination)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	
	// Custom director to modify the request
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetURL.Host
		req.URL.Host = targetURL.Host
		req.URL.Scheme = targetURL.Scheme
		
		// For path replacement, we want to replace the source prefix with the destination prefix
		sourcePath := strings.TrimSuffix(route.Source, "/")
		if strings.HasPrefix(req.URL.Path, sourcePath) {
			// Extract the remaining path after the source prefix
			remainingPath := strings.TrimPrefix(req.URL.Path, sourcePath)
			
			// Parse destination URL to get just the path part (not the full URL)
			destURL, _ := url.Parse(route.Destination)
			destPath := strings.TrimSuffix(destURL.Path, "/")
			
			// If destination path is empty or just "/", don't add extra path
			if destPath == "" || destPath == "/" {
				req.URL.Path = remainingPath
			} else {
				req.URL.Path = destPath + remainingPath
			}
		}
	}

	// Custom transport for logging
	proxy.Transport = &LoggingTransport{
		Transport: http.DefaultTransport,
		Config:    config,
		RouteName: route.Name,
	}

	return &RouteHandler{
		route:     route,
		proxy:     proxy,
		targetURL: targetURL,
	}, nil
}

func (s *ProxyServer) Start() {
	// Register route handlers
	for sourcePath, handler := range s.routes {
		// Remove trailing slash for pattern matching
		pattern := strings.TrimSuffix(sourcePath, "/") + "/"
		http.HandleFunc(pattern, handler.ServeHTTP)
		
		fmt.Printf("Route: %s -> %s (%s)\n", 
			sourcePath, handler.route.Destination, handler.route.Name)
	}

	addr := fmt.Sprintf("%s:%d", s.config.Server.Host, s.config.Server.Port)
	fmt.Printf("Proxy server starting on %s\n", addr)
	if s.config.Logging.File != "" {
		fmt.Printf("Logs will be saved to %s\n", s.config.Logging.File)
	}
	
	log.Fatal(http.ListenAndServe(addr, nil))
}

func (h *RouteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.proxy.ServeHTTP(w, r)
}

type LoggingTransport struct {
	Transport http.RoundTripper
	Config    *Config
	RouteName string
}

func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	
	// Read and log request body
	var requestBody []byte
	if req.Body != nil {
		requestBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(requestBody))
	}

	// Perform the actual request
	resp, err := t.Transport.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	// For streaming responses (SSE), we need to handle them differently
	isStreaming := isStreamingResponse(resp)
	var responseBody []byte
	
	if !isStreaming {
		// Read response body for logging (non-streaming)
		if resp.Body != nil {
			responseBody, _ = io.ReadAll(resp.Body)
			resp.Body = io.NopCloser(bytes.NewReader(responseBody))
		}
	} else {
		// For streaming, we'll create a tee reader to capture data as it flows
		resp.Body = &StreamingLogger{
			ReadCloser: resp.Body,
			Config:     t.Config,
			Timestamp:  start,
			Method:     req.Method,
			Path:       req.URL.Path,
			StatusCode: resp.StatusCode,
			RouteName:  t.RouteName,
			RequestBody: requestBody,
		}
		// Don't read the body here, let it stream through
	}

	duration := time.Since(start)

	// Log asynchronously to avoid blocking (for non-streaming responses)
	if !isStreaming {
		go t.logRequestResponse(start, req.Method, req.URL.Path, resp.StatusCode, duration, requestBody, responseBody)
	}

	return resp, nil
}

func (t *LoggingTransport) logRequestResponse(timestamp time.Time, method, path string, statusCode int, duration time.Duration, requestBody, responseBody []byte) {
	// Simple console and file log
	logLine := fmt.Sprintf("%s %s %s -> %d (%s) [%s]\n", 
		timestamp.Format("2006-01-02 15:04:05"), 
		method, 
		path, 
		statusCode, 
		duration,
		t.RouteName)
	
	if t.Config.Logging.Console {
		fmt.Print(logLine)
	}
	
	// Append to log file
	if t.Config.Logging.File != "" {
		if file, err := os.OpenFile(t.Config.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666); err == nil {
			file.WriteString(logLine)
			file.Close()
		}
	}

	// Create binary files for request/response data
	if t.Config.Logging.BinaryFiles {
		baseFilename := generateUniqueFilename(timestamp)
		
		// Save request
		if len(requestBody) > 0 {
			os.WriteFile(baseFilename+"-request.bin", requestBody, 0666)
		}
		
		// Save response
		if len(responseBody) > 0 {
			os.WriteFile(baseFilename+"-response.bin", responseBody, 0666)
		}
	}
}

func isStreamingResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	return strings.Contains(contentType, "text/event-stream") || 
		   strings.Contains(contentType, "application/stream") ||
		   resp.Header.Get("Transfer-Encoding") == "chunked"
}

// StreamingLogger wraps a ReadCloser to log streaming data
type StreamingLogger struct {
	io.ReadCloser
	Config      *Config
	Timestamp   time.Time
	Method      string
	Path        string
	StatusCode  int
	RouteName   string
	RequestBody []byte
	buffer      bytes.Buffer
	logged      bool
}

func (s *StreamingLogger) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	
	// Capture the data as it streams
	if n > 0 {
		s.buffer.Write(p[:n])
	}
	
	// If this is the end of the stream, log everything
	if err == io.EOF && !s.logged {
		s.logged = true
		duration := time.Since(s.Timestamp)
		
		logLine := fmt.Sprintf("%s %s %s -> %d (%s) [%s] STREAMING\n", 
			s.Timestamp.Format("2006-01-02 15:04:05"), 
			s.Method, 
			s.Path, 
			s.StatusCode, 
			duration,
			s.RouteName)
		
		if s.Config.Logging.Console {
			fmt.Print(logLine)
		}
		
		if s.Config.Logging.File != "" {
			if file, err := os.OpenFile(s.Config.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666); err == nil {
				file.WriteString(logLine)
				file.Close()
			}
		}

		// Save binary files
		if s.Config.Logging.BinaryFiles {
			baseFilename := generateUniqueFilename(s.Timestamp)
			
			if len(s.RequestBody) > 0 {
				os.WriteFile(baseFilename+"-request.bin", s.RequestBody, 0666)
			}
			
			if s.buffer.Len() > 0 {
				os.WriteFile(baseFilename+"-response.bin", s.buffer.Bytes(), 0666)
			}
		}
	}
	
	return n, err
}

func generateUniqueFilename(t time.Time) string {
	baseFormat := t.Format("2006-01-02_15-04-05")
	nanoseconds := t.Nanosecond()
	
	filename := fmt.Sprintf("%s-%09d", baseFormat, nanoseconds)
	
	// Check if files with this base name already exist, add counter if needed
	counter := 0
	for {
		testName := filename
		if counter > 0 {
			testName = fmt.Sprintf("%s_%d", filename, counter)
		}
		
		// Check if either request or response file exists
		requestExists := fileExists(testName + "-request.bin")
		responseExists := fileExists(testName + "-response.bin")
		
		if !requestExists && !responseExists {
			return testName
		}
		
		counter++
	}
}

func fileExists(filename string) bool {
	_, err := os.Stat(filename)
	return !os.IsNotExist(err)
}
