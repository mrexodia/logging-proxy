package loggingproxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ProxyServer struct {
	Config        *Config
	Routes        map[string]*Route
	loggingClient *LoggingClient
}

func NewProxyServer(config *Config) *ProxyServer {
	server := &ProxyServer{
		Config:        config,
		Routes:        make(map[string]*Route),
		loggingClient: NewLoggingClient(config.Logging.ServerURL),
	}

	// Initialize routes map from the config map
	for _, route := range config.Routes {
		routeCopy := route // Make a copy to avoid pointer issues
		server.Routes[route.Source] = &routeCopy
	}

	return server
}

// findMatchingRoute finds the best matching route for the given path
func (s *ProxyServer) findMatchingRoute(path string) *Route {
	var bestMatch *Route
	var bestLength int

	for sourcePath, route := range s.Routes {
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

func (s *ProxyServer) HandleRequest(w http.ResponseWriter, r *http.Request) {
	s.handleRequest(w, r)
}

func (s *ProxyServer) Start() {
	// Single handler for all requests
	http.HandleFunc("/", s.handleRequest)

	// Display configured routes
	for sourcePath, route := range s.Routes {
		loggingStatus := "logging disabled"
		if route.Logging {
			loggingStatus = "logging enabled"
		}
		fmt.Printf("Route: %s -> %s (%s)\n",
			sourcePath, route.Destination, loggingStatus)
	}

	addr := fmt.Sprintf("%s:%d", s.Config.Server.Host, s.Config.Server.Port)
	fmt.Printf("Proxy server starting on %s\n", addr)
	fmt.Printf("Logging server: %s\n", s.Config.Logging.ServerURL)

	log.Fatal(http.ListenAndServe(addr, nil))
}

// handleRequest processes incoming requests with streaming duplex architecture
func (s *ProxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Generate unique request ID
	requestID := uuid.New().String()

	// Find matching route
	route := s.findMatchingRoute(r.URL.Path)
	if route == nil {
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
			http.NotFound(w, r)
		}
		return
	}

	// Determine if this route should be logged
	shouldLog := route.Logging

	if s.Config.Logging.Console {
		loggingStatus := "no-log"
		if shouldLog {
			loggingStatus = "log"
		}
		fmt.Printf("%s [%s] %s %s -> %s [%s]\n",
			start.Format("2006-01-02 15:04:05"),
			requestID[:8],
			r.Method,
			r.URL.Path,
			route.Destination,
			loggingStatus)
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
	err = s.proxyWithDuplex(w, r, targetURL, requestID, route, shouldLog)
	if err != nil {
		log.Printf("Proxy error for %s: %v", requestID, err)
		http.Error(w, "Proxy error", http.StatusBadGateway)
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

			// Content-Length header - Go calculates this automatically
			// It may not be in req.Header but is available via req.ContentLength
			// Only add if not already in headers and we have a positive content length
			if req.ContentLength > 0 && req.Header.Get("Content-Length") == "" {
				fmt.Fprintf(pipeWriter, "Content-Length: %d\r\n", req.ContentLength)
			}

			// Transfer-Encoding header - Go handles this automatically
			// Usually not in req.Header when it's "chunked"
			if len(req.TransferEncoding) > 0 {
				fmt.Fprintf(pipeWriter, "Transfer-Encoding: %s\r\n", strings.Join(req.TransferEncoding, ", "))
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
	url := fmt.Sprintf("%s/%s/%s", s.loggingClient.serverURL, requestID, streamType)
	logReq, err := http.NewRequest("PUT", url, pipeReader)
	if err != nil {
		log.Printf("Error creating logging request for %s %s: %v", requestID, streamType, err)
		return
	}

	logReq.Header.Set("Content-Type", "application/octet-stream")

	logResp, err := s.loggingClient.client.Do(logReq)
	if err != nil {
		log.Printf("Error sending %s log for %s: %v", streamType, requestID, err)
		return
	}
	defer logResp.Body.Close()

	if logResp.StatusCode != http.StatusOK && logResp.StatusCode != http.StatusCreated {
		log.Printf("Logging server returned %d for %s %s", logResp.StatusCode, streamType, requestID)
	}
}
