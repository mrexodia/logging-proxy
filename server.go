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
	"golang.org/x/net/http/httpguts"
)

// RouteAuthorization configures an optional client credential check and an
// optional authorization value sent to the backend for a reverse proxy route.
// When only ClientValue is set, the validated client header is removed before
// forwarding.
type RouteAuthorization struct {
	Header       string
	BackendValue string
	ClientValue  string
}

// RouteOptions configures optional behavior for a reverse proxy route.
type RouteOptions struct {
	Authorization *RouteAuthorization
}

type ProxyServer struct {
	mux    *http.ServeMux
	client *http.Client
}

func NewProxyServer(notFoundEndpoint string) *ProxyServer {
	return newProxyServerWithClient(notFoundEndpoint, newDirectHTTPClient())
}

func NewProxyServerWithHTTPClientProxy(notFoundEndpoint string, proxyConfig HTTPClientProxyConfig) (*ProxyServer, error) {
	client, err := newHTTPClient(proxyConfig)
	if err != nil {
		return nil, err
	}
	return newProxyServerWithClient(notFoundEndpoint, client), nil
}

func newProxyServerWithClient(notFoundEndpoint string, client *http.Client) *ProxyServer {
	mux := http.NewServeMux()
	if notFoundEndpoint != "" {
		if !strings.HasSuffix(notFoundEndpoint, "/") {
			notFoundEndpoint += "/"
		}
		mux.HandleFunc(notFoundEndpoint, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, fmt.Sprintf("No route found for %s", r.URL.String()), http.StatusNotFound)
		})
	}
	if client == nil {
		client = newDirectHTTPClient()
	}
	return &ProxyServer{
		mux:    mux,
		client: client,
	}
}

// ServeHTTP implements http.Handler interface
func (s *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *ProxyServer) AddRoute(pattern string, destination string, logger Logger) error {
	return s.AddRouteWithOptions(pattern, destination, logger, RouteOptions{})
}

func (s *ProxyServer) AddRouteWithOptions(pattern string, destination string, logger Logger, options RouteOptions) error {
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

	authorization, err := validateRouteAuthorization(options.Authorization)
	if err != nil {
		return err
	}

	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if authorization != nil {
			if authorization.ClientValue != "" && r.Header.Get(authorization.Header) != authorization.ClientValue {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			if authorization.BackendValue == "" {
				r.Header.Del(authorization.Header)
			} else {
				if r.Header == nil {
					r.Header = make(http.Header)
				}
				r.Header.Set(authorization.Header, authorization.BackendValue)
			}
		}
		s.handleRequest(w, r, *destinationURL, logger)
	})

	return nil
}

func validateRouteAuthorization(authorization *RouteAuthorization) (*RouteAuthorization, error) {
	if authorization == nil {
		return nil, nil
	}

	validated := *authorization
	validated.Header = strings.TrimSpace(validated.Header)
	if validated.Header == "" {
		validated.Header = "Authorization"
	}
	if !httpguts.ValidHeaderFieldName(validated.Header) {
		return nil, fmt.Errorf("invalid authorization header name %q", validated.Header)
	}
	if validated.BackendValue == "" && validated.ClientValue == "" {
		return nil, fmt.Errorf("authorization requires a backend value, a client value, or both")
	}
	if validated.BackendValue != "" && !httpguts.ValidHeaderFieldValue(validated.BackendValue) {
		return nil, fmt.Errorf("authorization backend value is not a valid HTTP header value")
	}
	if validated.ClientValue != "" && !httpguts.ValidHeaderFieldValue(validated.ClientValue) {
		return nil, fmt.Errorf("authorization client value is not a valid HTTP header value")
	}
	return &validated, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func shouldSkipLoggedRequestHeader(name string) bool {
	return strings.EqualFold(name, "Host") ||
		strings.EqualFold(name, "Content-Encoding") ||
		strings.EqualFold(name, "Proxy-Authorization") ||
		strings.EqualFold(name, "Proxy-Authenticate")
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
		RequestStartedAt:       requestTime,
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

		// Write remaining headers, excluding hop-by-hop proxy auth and decompressed encoding headers.
		for name, values := range request.Header {
			if shouldSkipLoggedRequestHeader(name) {
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
	// Also store upstream response status and header latency.
	metadata.UpstreamResponseAt = &responseTime
	metadata.UpstreamHeaderDurationMS = responseTime.Sub(requestTime).Milliseconds()
	metadata.ResponseStatus = response.Status
	metadata.ResponseStatusCode = response.StatusCode
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
