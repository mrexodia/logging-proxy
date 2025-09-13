package loggingproxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

type ProxyServer struct {
	mux *http.ServeMux
}

func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		mux: http.NewServeMux(),
	}
}

// ServeHTTP implements http.Handler interface
func (s *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *ProxyServer) AddRoute(pattern, destination string, logger Logger) error {
	wildcardRegex := regexp.MustCompile(`{[a-zA-Z0-9_.]+`)

	if wildcardRegex.MatchString(pattern) {
		return fmt.Errorf("pattern %s contains a wildcard, which is not supported", pattern)
	}

	// Append a named wildcard so we can extract the path from the request
	if strings.HasSuffix(pattern, "/") {
		pattern += "{path...}"
	}

	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		s.handlePatternRequest(w, r, destination, logger)
	})

	return nil
}

func (s *ProxyServer) Start(addr string) error {
	server := &http.Server{
		Addr:                         addr,
		Handler:                      s,
		DisableGeneralOptionsHandler: true,
	}

	return server.ListenAndServe()
}

// handlePatternRequest processes requests that match a configured pattern
func (s *ProxyServer) handlePatternRequest(w http.ResponseWriter, r *http.Request, destination string, logger Logger) {
	requestID := uuid.New().String()

	// Extract path from the pattern match
	path := r.PathValue("path")
	destinationUrl := destination
	if len(path) > 0 {
		joined, err := url.JoinPath(destination, path)
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

	// Parse target URL
	targetURL, err := url.Parse(destinationUrl)
	if err != nil {
		http.Error(w, "Invalid destination URL", http.StatusInternalServerError)
		return
	}

	// Create the duplex streaming proxy request
	err = s.proxyWithDuplex(w, r, targetURL, requestID, logger)
	if err != nil {
		log.Printf("Proxy error for %s: %v", requestID, err)
		http.Error(w, "Proxy error", http.StatusBadGateway)
	}
}

// proxyWithDuplex handles the duplex streaming proxy logic with minimal latency
func (s *ProxyServer) proxyWithDuplex(w http.ResponseWriter, originalReq *http.Request, targetURL *url.URL, requestID string, logger Logger) error {
	// Create request metadata inline
	metadata := RequestMetadata{
		ID:        requestID,
		Pattern:   originalReq.Pattern,
		Method:    originalReq.Method,
		SourceURL: originalReq.URL.String(),
		TargetURL: targetURL.String(),
	}

	// Split request body stream for logging
	requestLogReader, requestLogWriter := io.Pipe()
	teeReader := io.TeeReader(originalReq.Body, requestLogWriter)
	defer originalReq.Body.Close()
	go logger.LogRequest(metadata, requestLogReader, originalReq)

	// Create and execute the proxy request synchronously
	proxyReq, err := http.NewRequest(originalReq.Method, targetURL.String(), teeReader)
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
	requestLogWriter.Close() // Close the request log writer after request is sent
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
	go logger.LogResponse(metadata, responseLogReader, resp)
	teeReader = io.TeeReader(resp.Body, responseLogWriter)
	_, err = io.Copy(w, teeReader)
	responseLogWriter.Close()

	return err
}
