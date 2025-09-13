package loggingproxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ProxyServer struct {
	mux    *http.ServeMux
	client *http.Client
}

func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		mux:    http.NewServeMux(),
		client: &http.Client{},
	}
}

// ServeHTTP implements http.Handler interface
func (s *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *ProxyServer) AddRoute(pattern string, destination string, logger Logger) error {
	// Make sure the pattern doesn't contain a wildcard
	wildcardRegex := regexp.MustCompile(`{[a-zA-Z0-9_.]+`)
	if wildcardRegex.MatchString(pattern) {
		return fmt.Errorf("pattern %s contains a wildcard, which is not supported", pattern)
	}

	// Append a named wildcard so we can extract the path from the request
	if strings.HasSuffix(pattern, "/") {
		pattern += "{path...}"
	}

	destinationURL, err := url.Parse(destination)
	if err != nil {
		return fmt.Errorf("failed to parse destination URL %q: %v", destination, err)
	}

	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		s.handleRequest(w, r, *destinationURL, logger)
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

type readCloser struct {
	io.Reader
	io.Closer
}

func (s *ProxyServer) handleRequest(w http.ResponseWriter, request *http.Request, destinationURL url.URL, logger Logger) {
	// Capture request data
	requestTime := time.Now()
	requestURI := request.RequestURI

	// Construct the target URL
	path := request.PathValue("path")
	if len(path) > 0 {
		destinationURL = *destinationURL.JoinPath(path)
	}
	if len(request.URL.RawQuery) > 0 {
		destinationURL.RawQuery = request.URL.RawQuery
	}

	// Create request metadata
	metadata := RequestMetadata{
		ID:             uuid.New().String(),
		Pattern:        request.Pattern,
		Method:         request.Method,
		SourceURL:      *request.URL,
		DestinationURL: destinationURL,
	}

	// Split request body stream for logging
	requestLogReader, requestLogWriter := io.Pipe()
	requestBody := readCloser{
		Reader: io.TeeReader(request.Body, requestLogWriter),
		Closer: request.Body,
	}
	defer requestBody.Close()

	// Modify the existing request to become the proxy request
	request.URL = &destinationURL
	request.Body = requestBody
	request.Host = destinationURL.Host
	request.RequestURI = "" // Must be empty in a client request

	// Async request logging with header reconstruction (log the outgoing proxy request)
	go func() {
		// Reconstruct proxy request headers
		var headerBuf bytes.Buffer

		// Write request line
		fmt.Fprintf(&headerBuf, "%s %s %s\r\n", request.Method, requestURI, request.Proto)

		// Write Host header
		if request.Host != "" {
			fmt.Fprintf(&headerBuf, "Host: %s\r\n", request.Host)
		}

		// Write remaining headers
		for name, values := range request.Header {
			for _, value := range values {
				fmt.Fprintf(&headerBuf, "%s: %s\r\n", name, value)
			}
		}

		// Write separator between headers and body
		headerBuf.WriteString("\r\n")

		// Combine headers + body from pipe
		logger.LogRequest(metadata, requestTime, &readCloser{
			Reader: io.MultiReader(&headerBuf, requestLogReader),
			Closer: requestLogReader,
		})

		// Close the pipe writer
		requestLogWriter.Close()
	}()

	// Execute the proxy request synchronously
	response, err := s.client.Do(request)
	if err != nil {
		// TODO: add a test case for this
		http.Error(w, fmt.Sprintf("[%s] proxy request failed: %v", metadata.ID, err), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	// Capture response timestamp
	responseTime := time.Now()

	// Send response headers as quickly as possible
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)

	// Split response stream for logging
	responseLogReader, responseLogWriter := io.Pipe()
	responseBody := io.TeeReader(response.Body, responseLogWriter)
	defer response.Body.Close()

	// Async response logging with header reconstruction
	go func() {
		// Reconstruct response headers
		var headerBuf bytes.Buffer

		// Write response status line
		fmt.Fprintf(&headerBuf, "%s %s\r\n", response.Proto, response.Status)

		// Write all response headers (no filtering)
		for name, values := range response.Header {
			for _, value := range values {
				fmt.Fprintf(&headerBuf, "%s: %s\r\n", name, value)
			}
		}

		// Write separator between headers and body
		headerBuf.WriteString("\r\n")

		// Combine headers + body from pipe
		logger.LogResponse(metadata, responseTime, &readCloser{
			Reader: io.MultiReader(&headerBuf, responseLogReader),
			Closer: responseLogReader,
		})

		// Close the pipe writer
		responseLogWriter.Close()
	}()

	// Stream the response body
	_, err = io.Copy(w, responseBody)
	if err != nil {
		// We already wrote the response, so we can't return an error to the client
		// TODO: when does this happen?
	}
}
