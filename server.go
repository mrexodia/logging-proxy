package loggingproxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
)

type ProxyServer struct {
	mux    *http.ServeMux
	client *http.Client
}

func NewProxyServer(notFoundEndpoint string) *ProxyServer {
	mux := http.NewServeMux()
	if notFoundEndpoint != "" {
		if !strings.HasSuffix(notFoundEndpoint, "/") {
			notFoundEndpoint += "/"
		}
		mux.HandleFunc(notFoundEndpoint, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, fmt.Sprintf("No route found for %s", r.URL.String()), http.StatusNotFound)
		})
	}
	return &ProxyServer{
		mux:    mux,
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

	// Go URLs support relative paths, but passing them to the http.Client after
	// JoinPath will result in an invalid HTTP request.
	// Issue: https://github.com/golang/go/issues/76635
	if destinationURL.Path == "" {
		destinationURL.Path = "/"
	}

	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		s.handleRequest(w, r, *destinationURL, logger)
	})

	return nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// decompressReader returns a reader that decompresses the input based on the Content-Encoding.
// If encoding is empty or unknown, it returns the original reader.
// Supports: gzip, deflate, br (brotli), compress, identity
func decompressReader(r io.Reader, encoding string) (io.ReadCloser, error) {
	// Normalize encoding (trim spaces, lowercase)
	encoding = strings.TrimSpace(strings.ToLower(encoding))

	// Handle empty or identity encoding (no compression)
	if encoding == "" || encoding == "identity" {
		return io.NopCloser(r), nil
	}

	// Handle multiple encodings (applied in order, so decompress in reverse)
	if strings.Contains(encoding, ",") {
		encodings := strings.Split(encoding, ",")
		// Decompress in reverse order (last encoding first)
		var err error
		currentReader := r
		for i := len(encodings) - 1; i >= 0; i-- {
			enc := strings.TrimSpace(encodings[i])
			var rc io.ReadCloser
			rc, err = decompressReader(currentReader, enc)
			if err != nil {
				return nil, fmt.Errorf("failed to decompress encoding %q: %w", enc, err)
			}
			currentReader = rc
		}
		return io.NopCloser(currentReader), nil
	}

	// Single encoding
	switch encoding {
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		return gr, nil

	case "deflate":
		// deflate is flate without the zlib wrapper
		return flate.NewReader(r), nil

	case "br":
		// Brotli compression
		return io.NopCloser(brotli.NewReader(r)), nil

	case "compress", "x-compress":
		// LZW compression (uncommon, not implementing for now)
		return nil, fmt.Errorf("compress/LZW encoding not supported")

	default:
		// Unknown encoding, return as-is
		return nil, fmt.Errorf("unknown encoding: %s", encoding)
	}
}

func (s *ProxyServer) handleRequest(w http.ResponseWriter, request *http.Request, destinationURL url.URL, logger Logger) {
	// Capture request data
	requestTime := time.Now()

	// Construct the full source URL (incoming request)
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	sourceURL := fmt.Sprintf("%s://%s%s", scheme, request.Host, request.URL.String())

	// Construct the target URL
	path := request.PathValue("path")
	if len(path) > 0 {
		destinationURL = *destinationURL.JoinPath(path)
	}
	if len(request.URL.RawQuery) > 0 {
		destinationURL.RawQuery = request.URL.RawQuery
	}

	// Capture request Content-Encoding before modifying the request
	requestContentEncoding := request.Header.Get("Content-Encoding")

	// Create request metadata
	metadata := RequestMetadata{
		ID:                     uuid.New().String(),
		Pattern:                request.Pattern,
		Method:                 request.Method,
		SourceURL:              sourceURL,
		DestinationURL:         destinationURL.String(),
		RequestContentEncoding: requestContentEncoding,
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
		defer requestLogReader.Close()

		// Reconstruct proxy request headers
		var headerBuf bytes.Buffer

		// Write request line with full destination URL
		fmt.Fprintf(&headerBuf, "%s %s %s\r\n", request.Method, destinationURL.String(), request.Proto)

		// Write remaining headers (skip Host and Content-Encoding headers)
		for name, values := range request.Header {
			// Skip Host header (URL is absolute) and Content-Encoding (we're logging decompressed)
			if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Content-Encoding") {
				continue
			}
			for _, value := range values {
				fmt.Fprintf(&headerBuf, "%s: %s\r\n", name, value)
			}
		}

		// Write separator between headers and body
		headerBuf.WriteString("\r\n")

		// Decompress the request body if needed
		var bodyReader io.Reader = requestLogReader
		if requestContentEncoding != "" {
			decompressed, err := decompressReader(requestLogReader, requestContentEncoding)
			if err != nil {
				// If decompression fails, log the compressed data as-is
				fmt.Fprintf(&headerBuf, "X-Decompression-Error: %v\r\n", err)
			} else {
				defer decompressed.Close()
				bodyReader = decompressed
			}
		}

		// Combine headers + body
		logger.LogRequest(metadata, requestTime, &readCloser{
			Reader: io.MultiReader(&headerBuf, bodyReader),
			Closer: io.NopCloser(nil), // The pipe closer is already deferred
		})
	}()

	// Execute the proxy request synchronously
	response, err := s.client.Do(request)

	// Close the request writer now that request body has been consumed
	requestLogWriter.Close()

	if err != nil {
		// TODO: add a test case for this
		http.Error(w, fmt.Sprintf("[%s] proxy request failed: %v", metadata.ID, err), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	// Capture response timestamp and Content-Encoding
	responseTime := time.Now()
	responseContentEncoding := response.Header.Get("Content-Encoding")

	// Update metadata with response encoding
	metadata.ResponseContentEncoding = responseContentEncoding

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
		defer responseLogReader.Close()

		// Reconstruct response headers
		var headerBuf bytes.Buffer

		// Write response status line
		fmt.Fprintf(&headerBuf, "%s %s\r\n", response.Proto, response.Status)

		// Write response headers (skip Content-Encoding as we're logging decompressed)
		for name, values := range response.Header {
			if strings.EqualFold(name, "Content-Encoding") {
				continue
			}
			for _, value := range values {
				fmt.Fprintf(&headerBuf, "%s: %s\r\n", name, value)
			}
		}

		// Write separator between headers and body
		headerBuf.WriteString("\r\n")

		// Decompress the response body if needed
		var bodyReader io.Reader = responseLogReader
		if responseContentEncoding != "" {
			decompressed, err := decompressReader(responseLogReader, responseContentEncoding)
			if err != nil {
				// If decompression fails, log the compressed data as-is
				fmt.Fprintf(&headerBuf, "X-Decompression-Error: %v\r\n", err)
			} else {
				defer decompressed.Close()
				bodyReader = decompressed
			}
		}

		// Combine headers + body
		logger.LogResponse(metadata, responseTime, &readCloser{
			Reader: io.MultiReader(&headerBuf, bodyReader),
			Closer: io.NopCloser(nil), // The pipe closer is already deferred
		})
	}()

	// Stream the response body (no error checking, because we already wrote the response)
	io.Copy(w, responseBody)

	// Close the response writer now that response body has been consumed
	responseLogWriter.Close()
}
