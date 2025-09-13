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
		Console   bool   `yaml:"console"`
		ServerURL string `yaml:"server_url"`
		Default   bool   `yaml:"default"`
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
	Logging     bool   `yaml:"logging"`
}

type ProxyServer struct {
	Config        *Config
	loggingClient *http.Client
	mux           *http.ServeMux
}

func NewProxyServer(config *Config) *ProxyServer {
	server := &ProxyServer{
		Config:        config,
		loggingClient: &http.Client{Timeout: 30 * time.Second},
		mux:           http.NewServeMux(),
	}

	// Setup HTTP patterns
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
		loggingStatus := "logging disabled"
		if route.Logging {
			loggingStatus = "logging enabled"
		}
		fmt.Printf("Route: %s -> %s (%s)\n",
			route.Pattern, route.Destination, loggingStatus)
	}

	addr := fmt.Sprintf("%s:%d", s.Config.Server.Host, s.Config.Server.Port)
	fmt.Printf("Proxy server starting on %s\n", addr)
	fmt.Printf("Logging server: %s\n", s.Config.Logging.ServerURL)

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
		loggingStatus := "no-log"
		if route.Logging {
			loggingStatus = "log"
		}
		fmt.Printf("%s [%s] %s %s -> %s [%s]\n",
			start.Format("2006-01-02 15:04:05"),
			requestID[:8],
			r.Method,
			r.URL.Path,
			destinationUrl,
			loggingStatus)
	}

	// Parse target URL
	targetURL, err := url.Parse(destinationUrl)
	if err != nil {
		http.Error(w, "Invalid destination URL", http.StatusInternalServerError)
		return
	}

	// Create the duplex streaming proxy request
	err = s.proxyWithDuplex(w, r, targetURL, requestID, &route, route.Logging)
	if err != nil {
		log.Printf("Proxy error for %s: %v", requestID, err)
		http.Error(w, "Proxy error", http.StatusBadGateway)
	}
}

// handleUnknownRoute handles requests that don't match any configured pattern
func (s *ProxyServer) handleUnknownRoute(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := uuid.New().String()

	// Log unknown route to console if console logging is enabled
	if s.Config.Logging.Console {
		loggingStatus := "no-log"
		if s.Config.Logging.Default {
			loggingStatus = "log"
		}
		fmt.Printf("%s [%s] %s %s -> NOT_FOUND [%s]\n",
			start.Format("2006-01-02 15:04:05"),
			requestID[:8],
			r.Method,
			r.URL.Path,
			loggingStatus)
	}

	// If default logging is enabled, log the 404 request/response
	if s.Config.Logging.Default {
		s.logUnknownRoute(w, r, requestID)
	} else {
		http.Error(w, "custom 404 page", http.StatusNotFound)
	}
}

// logUnknownRoute handles logging for routes that are not found
func (s *ProxyServer) logUnknownRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	// Create a fake 404 response for logging
	proxyPath := fmt.Sprintf("http://%s:%d%s", s.Config.Server.Host, s.Config.Server.Port, r.URL.Path)
	if r.URL.RawQuery != "" {
		proxyPath += "?" + r.URL.RawQuery
	}

	// Log the request
	go s.streamToLoggingServer(r.Body, requestID, "request", r, proxyPath, nil)

	// Create and log the 404 response
	w.WriteHeader(http.StatusNotFound)
	responseBody := "404 page not found\n"

	// Create a fake response for logging
	fakeResp := &http.Response{
		Proto:      "HTTP/1.1",
		StatusCode: http.StatusNotFound,
		Header:     w.Header(),
	}

	// Log the 404 response
	go s.streamToLoggingServer(strings.NewReader(responseBody), requestID, "response", nil, "", fakeResp)

	// Write the actual response
	w.Write([]byte(responseBody))
}

// proxyWithDuplex handles the duplex streaming proxy logic
func (s *ProxyServer) proxyWithDuplex(w http.ResponseWriter, originalReq *http.Request, targetURL *url.URL, requestID string, route *Route, shouldLog bool) error {
	// Create the request to the target server
	proxyReq, err := http.NewRequest(originalReq.Method, targetURL.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create proxy request: %w", err)
	}

	// Copy headers from original request, updating Host header value
	for key, values := range originalReq.Header {
		if strings.ToLower(key) == "host" {
			// Update Host header to point to destination
			proxyReq.Header.Set(key, targetURL.Host)
		} else {
			for _, value := range values {
				proxyReq.Header.Add(key, value)
			}
		}
	}

	// Ensure Host field is also set (Go HTTP client requirement)
	proxyReq.Host = targetURL.Host

	// Handle request body and logging
	if shouldLog {
		// Construct the original proxy path for the X-Proxy-Path header
		proxyPath := fmt.Sprintf("http://%s:%d%s", s.Config.Server.Host, s.Config.Server.Port, originalReq.URL.Path)
		if originalReq.URL.RawQuery != "" {
			proxyPath += "?" + originalReq.URL.RawQuery
		}

		if originalReq.Body != nil {
			// Create a pipe for logging the request body
			logPipeReader, logPipeWriter := io.Pipe()

			// Start async logging
			go s.streamToLoggingServer(logPipeReader, requestID, "request", proxyReq, proxyPath, nil)

			// Use TeeReader to duplicate the stream
			teeReader := io.TeeReader(originalReq.Body, logPipeWriter)
			proxyReq.Body = &requestBodyCloser{
				Reader:     teeReader,
				original:   originalReq.Body,
				pipeWriter: logPipeWriter,
			}
		} else {
			// Log request without body
			go s.streamToLoggingServer(nil, requestID, "request", proxyReq, proxyPath, nil)
			proxyReq.Body = originalReq.Body
		}
	} else {
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

	// Handle response body and logging
	if shouldLog {
		// Create a pipe for logging the response body
		logPipeReader, logPipeWriter := io.Pipe()

		// Start async logging of response
		go s.streamToLoggingServer(logPipeReader, requestID, "response", nil, "", resp)

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

// streamToLoggingServer sends HTTP request/response data to the logging server
func (s *ProxyServer) streamToLoggingServer(reader io.Reader, requestID, streamType string, req *http.Request, proxyPath string, resp *http.Response) {
	// Create a pipe for streaming the data
	pipeReader, pipeWriter := io.Pipe()

	// Start a goroutine to write the HTTP data to the pipe
	go func() {
		defer pipeWriter.Close()

		if streamType == "request" && req != nil {
			// Write request line
			requestURI := req.URL.Path
			if req.URL.RawQuery != "" {
				requestURI += "?" + req.URL.RawQuery
			}
			fmt.Fprintf(pipeWriter, "%s %s %s\r\n", req.Method, requestURI, req.Proto)

			// Write headers that Go handles specially (not in req.Header map)

			// Host header - always available via req.Host
			if req.Host != "" {
				fmt.Fprintf(pipeWriter, "Host: %s\r\n", req.Host)
			}

			// Write other headers preserving original order but with updated values
			for name, values := range req.Header {
				// Skip headers we handle specially above
				lowerName := strings.ToLower(name)
				if lowerName != "host" && lowerName != "content-length" && lowerName != "transfer-encoding" {
					for _, value := range values {
						fmt.Fprintf(pipeWriter, "%s: %s\r\n", name, value)
					}
				}
			}

			// Add custom proxy path header for replay capability
			if proxyPath != "" {
				fmt.Fprintf(pipeWriter, "X-Proxy-Path: %s\r\n", proxyPath)
			}

		} else if streamType == "response" && resp != nil {
			// Write response status line
			statusText := http.StatusText(resp.StatusCode)
			fmt.Fprintf(pipeWriter, "%s %d %s\r\n", resp.Proto, resp.StatusCode, statusText)

			// Write headers preserving original order
			for name, values := range resp.Header {
				for _, value := range values {
					fmt.Fprintf(pipeWriter, "%s: %s\r\n", name, value)
				}
			}
		}

		fmt.Fprintf(pipeWriter, "\r\n") // Empty line separating headers from body

		// Stream the body if present
		if reader != nil {
			_, err := io.Copy(pipeWriter, reader)
			if err != nil {
				log.Printf("Error streaming %s data for %s: %v", streamType, requestID, err)
			}
		}
	}()

	// Send to logging server using the pipe
	url := fmt.Sprintf("%s/%s/%s", s.Config.Logging.ServerURL, requestID, streamType)
	logReq, err := http.NewRequest("PUT", url, pipeReader)
	if err != nil {
		log.Printf("Error creating logging request for %s %s: %v", requestID, streamType, err)
		return
	}

	logReq.Header.Set("Content-Type", "application/octet-stream")

	logResp, err := s.loggingClient.Do(logReq)
	if err != nil {
		log.Printf("Error sending %s log for %s: %v", streamType, requestID, err)
		return
	}
	defer logResp.Body.Close()

	if logResp.StatusCode != http.StatusOK && logResp.StatusCode != http.StatusCreated {
		log.Printf("Logging server returned %d for %s %s", logResp.StatusCode, streamType, requestID)
	}
}
