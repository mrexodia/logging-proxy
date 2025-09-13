package loggingproxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
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
	Routes map[string]Route `yaml:"routes"`
}

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

type ProxyServer struct {
	Config *Config
	logger Logger
	mux    *http.ServeMux
}

func NewProxyServer(config *Config) *ProxyServer {
	var logger Logger = &NoOpLogger{}
	
	if config.Logging.File {
		logDir := config.Logging.LogDir
		if logDir == "" {
			logDir = "logs"
		}
		if fileLogger, err := NewFileLogger(logDir); err != nil {
			log.Printf("Failed to create file logger: %v, using no-op logger", err)
		} else {
			logger = fileLogger
		}
	}
	
	server := &ProxyServer{
		Config: config,
		logger: logger,
		mux:    http.NewServeMux(),
	}
	server.setupPatterns()
	return server
}

// setupPatterns configures HTTP patterns like experiment.go
func (s *ProxyServer) setupPatterns() {
	wildcardRegex := regexp.MustCompile(`{[a-zA-Z0-9_.]+`)
	registerCatchAll := true

	for _, route := range s.Config.Routes {
		pattern := route.Pattern
		fmt.Printf("[route] %s -> %s\n", pattern, route.Destination)

		if wildcardRegex.MatchString(pattern) {
			panic(fmt.Sprintf("Pattern %s contains a wildcard, which is not supported\n", pattern))
		}

		// If the user specifies a catch-all route, we don't need to register our own handler
		if pattern == "/" {
			registerCatchAll = false
		}

		// Append a named wildcard so we can extract the path from the request
		if strings.HasSuffix(pattern, "/") {
			pattern += "{path...}"
		}

		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.handlePatternRequest(w, r, route)
		})
	}

	if registerCatchAll {
		fmt.Printf("Registering catch-all handler\n")
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			s.handleUnknownRoute(w, r)
		})
	} else {
		fmt.Printf("Skipping catch-all handler\n")
	}
}

func (s *ProxyServer) Start() {
	// Display configured routes
	for _, route := range s.Config.Routes {
		fmt.Printf("Route: %s -> %s\n", route.Pattern, route.Destination)
	}

	addr := fmt.Sprintf("%s:%d", s.Config.Server.Host, s.Config.Server.Port)
	fmt.Printf("Proxy server starting on %s\n", addr)
	
	if s.Config.Logging.File {
		logDir := s.Config.Logging.LogDir
		if logDir == "" {
			logDir = "logs"
		}
		fmt.Printf("Logging: file logging to %s\n", logDir)
	}

	server := &http.Server{
		Addr:                         addr,
		Handler:                      s.mux,
		DisableGeneralOptionsHandler: true,
	}

	log.Fatal(server.ListenAndServe())
}

// handlePatternRequest processes requests that match a configured pattern
func (s *ProxyServer) handlePatternRequest(w http.ResponseWriter, r *http.Request, route Route) {
	start := time.Now()
	requestID := uuid.New().String()

	// Extract path from the pattern match
	path := r.PathValue("path")
	destinationUrl := route.Destination
	if len(path) > 0 {
		joined, err := url.JoinPath(route.Destination, path)
		if err != nil {
			log.Printf("Error joining path: %v", err)
			http.Error(w, "Invalid destination URL", http.StatusInternalServerError)
			return
		}
		destinationUrl = joined
	}

	if len(r.URL.RawQuery) > 0 {
		destinationUrl += "?" + r.URL.RawQuery
	}

	// Console logging
	if s.Config.Logging.Console {
		fmt.Printf("%s [%s] %s %s -> %s\n",
			start.Format("2006-01-02 15:04:05"),
			requestID[:8],
			r.Method,
			r.URL.Path,
			destinationUrl)
	}

	// Parse target URL
	targetURL, err := url.Parse(destinationUrl)
	if err != nil {
		http.Error(w, "Invalid destination URL", http.StatusInternalServerError)
		return
	}

	// Create the duplex streaming proxy request
	err = s.proxyWithDuplex(w, r, targetURL, requestID, &route)
	if err != nil {
		log.Printf("Proxy error for %s: %v", requestID, err)
		http.Error(w, "Proxy error", http.StatusBadGateway)
	}
}

// handleUnknownRoute handles requests that don't match any configured pattern
func (s *ProxyServer) handleUnknownRoute(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := uuid.New().String()

	// Console logging for unknown routes
	if s.Config.Logging.Console {
		fmt.Printf("%s [%s] %s %s -> NOT_FOUND\n",
			start.Format("2006-01-02 15:04:05"),
			requestID[:8],
			r.Method,
			r.URL.Path)
	}

	// Return 404 for unknown routes
	http.Error(w, "custom 404 page", http.StatusNotFound)
}


// createRequestMetadata creates metadata for a request to avoid duplication
func (s *ProxyServer) createRequestMetadata(r *http.Request, requestID string, route *Route) RequestMetadata {
	proxyPath := fmt.Sprintf("http://%s:%d%s", s.Config.Server.Host, s.Config.Server.Port, r.URL.Path)
	if r.URL.RawQuery != "" {
		proxyPath += "?" + r.URL.RawQuery
	}
	
	return RequestMetadata{
		ID:         requestID,
		Method:     r.Method,
		URL:        r.URL.String(),
		RemoteAddr: r.RemoteAddr,
		UserAgent:  r.UserAgent(),
		ProxyPath:  proxyPath,
		Route:      route,
	}
}

// proxyWithDuplex handles the duplex streaming proxy logic with minimal latency
func (s *ProxyServer) proxyWithDuplex(w http.ResponseWriter, originalReq *http.Request, targetURL *url.URL, requestID string, route *Route) error {
	metadata := s.createRequestMetadata(originalReq, requestID, route)

	// Split request body stream for logging
	requestLogReader, requestLogWriter := io.Pipe()
	teeReader := io.TeeReader(originalReq.Body, requestLogWriter)
	proxyRequestBody := &streamDuplexer{
		Reader:     teeReader,
		original:   originalReq.Body,
		logWriter:  requestLogWriter,
	}
	go s.logger.LogRequest(metadata, requestLogReader, originalReq)

	// Create and execute the proxy request synchronously  
	proxyReq, err := http.NewRequest(originalReq.Method, targetURL.String(), proxyRequestBody)
	if err != nil {
		return fmt.Errorf("failed to create proxy request: %w", err)
	}

	// Copy headers
	for key, values := range originalReq.Header {
		for _, value := range values {
			proxyReq.Header.Add(key, value)
		}
	}
	proxyReq.Host = targetURL.Host

	// Execute the proxy request synchronously
	client := &http.Client{}
	resp, err := client.Do(proxyReq)
	if err != nil {
		return fmt.Errorf("proxy request failed: %w", err)
	}
	defer resp.Body.Close()

	// Copy response headers immediately
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Split response stream for logging
	responseLogReader, responseLogWriter := io.Pipe()
	go s.logger.LogResponse(metadata, responseLogReader, resp)
	teeReader = io.TeeReader(resp.Body, responseLogWriter)
	_, err = io.Copy(w, teeReader)
	responseLogWriter.Close()

	return err
}



// streamDuplexer wraps a TeeReader and handles proper cleanup for duplexed streams
type streamDuplexer struct {
	io.Reader
	original  io.ReadCloser
	logWriter *io.PipeWriter
}

func (s *streamDuplexer) Close() error {
	// Close the log writer to signal end of stream to async logger
	s.logWriter.Close()
	// Close the original body
	return s.original.Close()
}


