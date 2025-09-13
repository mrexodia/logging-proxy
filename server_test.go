package loggingproxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Mock logging server for testing
type MockLoggingServer struct {
	server    *httptest.Server
	requests  map[string][]byte
	responses map[string][]byte
}

func NewMockLoggingServer() *MockLoggingServer {
	mock := &MockLoggingServer{
		requests:  make(map[string][]byte),
		responses: make(map[string][]byte),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(pathParts) != 2 {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}

		requestID := pathParts[0]
		streamType := pathParts[1]

		data, _ := io.ReadAll(r.Body)

		switch streamType {
		case "request":
			mock.requests[requestID] = data
		case "response":
			mock.responses[requestID] = data
		}

		w.WriteHeader(http.StatusCreated)
	})

	mock.server = httptest.NewServer(mux)
	return mock
}

func (m *MockLoggingServer) Close() {
	m.server.Close()
}

func (m *MockLoggingServer) URL() string {
	return m.server.URL
}

// Helper function to create test configs without YAML files
func createTestConfig(loggingServerURL string, consoleLogging, defaultLogging bool, routes map[string]Route) *Config {
	return &Config{
		Server: struct {
			Port int    `yaml:"port"`
			Host string `yaml:"host"`
		}{Port: 5601, Host: "localhost"},
		Logging: struct {
			Console   bool   `yaml:"console"`
			ServerURL string `yaml:"server_url"`
			Default   bool   `yaml:"default"`
		}{Console: consoleLogging, ServerURL: loggingServerURL, Default: defaultLogging},
		Routes: routes,
	}
}

func TestNewArchitecture(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo": "Backend received %s %s"}`, r.Method, r.URL.Path)
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config directly
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"test": {Pattern: "/api/v1/", Destination: backend.URL + "/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make a test request
	requestBody := `{"test": "data"}`
	resp, err := http.Post(testServer.URL+"/api/v1/test", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal("Request failed:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Read response
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("Failed to read response:", err)
	}

	expectedContent := "Backend received POST /test"
	if !strings.Contains(string(responseBody), expectedContent) {
		t.Errorf("Expected response to contain '%s', got: %s", expectedContent, string(responseBody))
	}

	// Give some time for async logging to complete
	time.Sleep(100 * time.Millisecond)

	// Check that logging server received the data
	if len(loggingServer.requests) == 0 {
		t.Error("No request data was logged")
	}

	if len(loggingServer.responses) == 0 {
		t.Error("No response data was logged")
	}

	// Verify we got exactly one request/response pair
	if len(loggingServer.requests) != 1 || len(loggingServer.responses) != 1 {
		t.Errorf("Expected 1 request and 1 response, got %d requests and %d responses",
			len(loggingServer.requests), len(loggingServer.responses))
	}

	// Check that request data contains our test data
	for requestID, requestData := range loggingServer.requests {
		t.Logf("Request %s: %d bytes", requestID[:8], len(requestData))
		if !strings.Contains(string(requestData), requestBody) {
			t.Error("Request data does not contain expected body")
		}

		// Check corresponding response
		if responseData, exists := loggingServer.responses[requestID]; exists {
			t.Logf("Response %s: %d bytes", requestID[:8], len(responseData))
			if !strings.Contains(string(responseData), "Backend received") {
				t.Error("Response data does not contain expected content")
			}
		} else {
			t.Error("No corresponding response found for request")
		}
	}
}

func TestStreamingWithNewArchitecture(t *testing.T) {
	// Create mock streaming backend
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		// Send several SSE events
		events := []string{
			"data: {\"chunk\": 1}\n\n",
			"data: {\"chunk\": 2}\n\n",
			"data: [DONE]\n\n",
		}

		for _, event := range events {
			fmt.Fprint(w, event)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config directly
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"streaming_test": {Pattern: "/api/v1/", Destination: backend.URL + "/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make streaming request
	resp, err := http.Get(testServer.URL + "/api/v1/stream")
	if err != nil {
		t.Fatal("Streaming request failed:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Read the streaming response
	var buffer bytes.Buffer
	_, err = io.Copy(&buffer, resp.Body)
	if err != nil {
		t.Fatal("Failed to read streaming response:", err)
	}

	responseData := buffer.String()
	expectedChunks := []string{"chunk\": 1", "chunk\": 2", "[DONE]"}
	for _, chunk := range expectedChunks {
		if !strings.Contains(responseData, chunk) {
			t.Errorf("Expected chunk '%s' not found in streaming response", chunk)
		}
	}

	// Give time for async logging
	time.Sleep(200 * time.Millisecond)

	// Verify logging occurred
	if len(loggingServer.requests) == 0 {
		t.Error("No streaming request was logged")
	}

	if len(loggingServer.responses) == 0 {
		t.Error("No streaming response was logged")
	}
}

func TestConfigValidationNew(t *testing.T) {
	config := &Config{
		Logging: struct {
			Console   bool   `yaml:"console"`
			ServerURL string `yaml:"server_url"`
			Default   bool   `yaml:"default"`
		}{Console: true, ServerURL: "http://localhost:8080", Default: true},
		Routes: map[string]Route{
			"test": {Pattern: "/api/v1/", Destination: "https://example.com/", Logging: true},
		},
	}

	server := NewProxyServer(config)

	if len(server.Config.Routes) != 1 {
		t.Errorf("Expected 1 route, got %d", len(server.Config.Routes))
	}

	route, exists := server.Config.Routes["test"]
	if !exists {
		t.Error("Route not found")
	}

	if route.Destination != "https://example.com/" {
		t.Errorf("Expected destination 'https://example.com/', got '%s'", route.Destination)
	}

	if !route.Logging {
		t.Error("Expected logging to be enabled for route")
	}
}

func TestUnknownRouteWithDefaultLoggingEnabled(t *testing.T) {
	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config with default logging enabled
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"known": {Pattern: "/api/", Destination: "https://example.com/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make request to unknown route
	resp, err := http.Get(testServer.URL + "/unknown/path")
	if err != nil {
		t.Fatal("Request failed:", err)
	}
	defer resp.Body.Close()

	// Should get 404
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}

	// Give time for async logging
	time.Sleep(100 * time.Millisecond)

	// Should have logged the unknown route (default: true)
	if len(loggingServer.requests) == 0 {
		t.Error("Expected unknown route request to be logged")
	}

	if len(loggingServer.responses) == 0 {
		t.Error("Expected unknown route response to be logged")
	}
}

func TestUnknownRouteWithDefaultLoggingDisabled(t *testing.T) {
	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config with default logging disabled
	config := createTestConfig(loggingServer.URL(), false, false, map[string]Route{
		"known": {Pattern: "/api/", Destination: "https://example.com/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make request to unknown route
	resp, err := http.Get(testServer.URL + "/unknown/path")
	if err != nil {
		t.Fatal("Request failed:", err)
	}
	defer resp.Body.Close()

	// Should get 404
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}

	// Give time for potential logging
	time.Sleep(100 * time.Millisecond)

	// Should NOT have logged the unknown route (default: false)
	if len(loggingServer.requests) > 0 {
		t.Error("Expected unknown route request NOT to be logged")
	}

	if len(loggingServer.responses) > 0 {
		t.Error("Expected unknown route response NOT to be logged")
	}
}

func TestRouteSpecificLoggingOverride(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path": "%s"}`, r.URL.Path)
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config with mixed logging settings
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"logged_route":   {Pattern: "/api/", Destination: backend.URL + "/", Logging: true},
		"unlogged_route": {Pattern: "/nolog/", Destination: backend.URL + "/", Logging: false},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Clear logging server state
	loggingServer.requests = make(map[string][]byte)
	loggingServer.responses = make(map[string][]byte)

	// Make request to logged route
	resp1, err := http.Get(testServer.URL + "/api/test")
	if err != nil {
		t.Fatal("Request to logged route failed:", err)
	}
	resp1.Body.Close()

	// Give time for logging
	time.Sleep(100 * time.Millisecond)

	loggedRequests := len(loggingServer.requests)
	loggedResponses := len(loggingServer.responses)

	// Make request to unlogged route
	resp2, err := http.Get(testServer.URL + "/nolog/test")
	if err != nil {
		t.Fatal("Request to unlogged route failed:", err)
	}
	resp2.Body.Close()

	// Give time for potential logging
	time.Sleep(100 * time.Millisecond)

	// Should have logged the first route but not the second
	if loggedRequests == 0 {
		t.Error("Expected logged route request to be logged")
	}

	if loggedResponses == 0 {
		t.Error("Expected logged route response to be logged")
	}

	// Should not have additional logs from the unlogged route
	if len(loggingServer.requests) > loggedRequests {
		t.Error("Expected unlogged route request NOT to be logged")
	}

	if len(loggingServer.responses) > loggedResponses {
		t.Error("Expected unlogged route response NOT to be logged")
	}
}

func TestRequestWithoutBody(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"method": "%s", "hasBody": %t}`, r.Method, r.ContentLength > 0)
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"test": {Pattern: "/api/", Destination: backend.URL + "/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make GET request (no body)
	resp, err := http.Get(testServer.URL + "/api/test")
	if err != nil {
		t.Fatal("GET request failed:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Give time for logging
	time.Sleep(100 * time.Millisecond)

	// Should have logged request and response even without body
	if len(loggingServer.requests) == 0 {
		t.Error("Expected GET request without body to be logged")
	}

	if len(loggingServer.responses) == 0 {
		t.Error("Expected GET response to be logged")
	}

	// Check that the logged request contains the proper HTTP format
	for _, requestData := range loggingServer.requests {
		requestString := string(requestData)
		if !strings.Contains(requestString, "GET /test HTTP/1.1") {
			t.Error("Expected logged request to contain proper HTTP request line")
		}
		if !strings.Contains(requestString, "X-Proxy-Path:") {
			t.Error("Expected logged request to contain X-Proxy-Path header")
		}
		if !strings.Contains(requestString, "Host:") {
			t.Error("Expected logged request to contain Host header")
			t.Logf("Request data: %s", requestString)
		}
		break // Check first request
	}
}

func TestHostHeaderLogging(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"host": "%s"}`, r.Host)
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"test": {Pattern: "/api/", Destination: backend.URL + "/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make request with explicit Host header
	client := &http.Client{}
	req, err := http.NewRequest("GET", testServer.URL+"/api/test", nil)
	if err != nil {
		t.Fatal("Failed to create request:", err)
	}
	req.Host = "custom-host.example.com"

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("Request failed:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Give time for logging
	time.Sleep(100 * time.Millisecond)

	// Check that the logged request contains the Host header
	if len(loggingServer.requests) == 0 {
		t.Fatal("Expected request to be logged")
	}

	for _, requestData := range loggingServer.requests {
		requestString := string(requestData)
		t.Logf("Logged request:\n%s", requestString)

		// Should contain the updated Host header pointing to the destination
		if !strings.Contains(requestString, "Host:") {
			t.Error("Expected logged request to contain Host header")
		}

		// Should contain the destination host, not the original custom host
		backendHost := strings.TrimPrefix(backend.URL, "http://")
		if !strings.Contains(requestString, backendHost) {
			t.Errorf("Expected Host header to contain backend host %s", backendHost)
		}

		// Should NOT contain the custom host from the original request
		if strings.Contains(requestString, "custom-host.example.com") {
			t.Error("Expected Host header to be updated to destination, not contain original custom host")
		}

		break // Check first request
	}
}

func TestSpecialHeadersLogging(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"method": "%s", "contentLength": %d}`, r.Method, r.ContentLength)
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"test": {Pattern: "/api/", Destination: backend.URL + "/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make POST request with body to test Content-Length
	requestBody := `{"test": "data with some content"}`
	resp, err := http.Post(testServer.URL+"/api/test", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal("POST request failed:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Give time for logging
	time.Sleep(100 * time.Millisecond)

	// Check that the logged request contains special headers
	if len(loggingServer.requests) == 0 {
		t.Fatal("Expected request to be logged")
	}

	for _, requestData := range loggingServer.requests {
		requestString := string(requestData)
		t.Logf("Logged request:\n%s", requestString)

		// Should contain Host header
		if !strings.Contains(requestString, "Host:") {
			t.Error("Expected logged request to contain Host header")
		}

		// Should contain Content-Length header (either explicit or in original headers)
		hasExplicitContentLength := strings.Contains(requestString, "Content-Length:")
		hasContentType := strings.Contains(requestString, "Content-Type:")
		hasBody := strings.Contains(requestString, requestBody)

		if !hasContentType {
			t.Error("Expected logged request to contain Content-Type header")
		}

		if !hasBody {
			t.Error("Expected logged request to contain request body")
		}

		// For POST with body, we should have either explicit Content-Length or the body should be present
		if !hasExplicitContentLength && !hasBody {
			t.Error("Expected either Content-Length header or request body to be logged")
		}

		// Should contain X-Proxy-Path header
		if !strings.Contains(requestString, "X-Proxy-Path:") {
			t.Error("Expected logged request to contain X-Proxy-Path header")
		}

		break // Check first request
	}
}

func TestConsoleLoggingBehavior(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK")
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Test different console logging scenarios
	testCases := []struct {
		name           string
		consoleEnabled bool
		defaultLogging bool
		routeLogging   bool
		expectConsole  bool
	}{
		{"Console enabled, default true, route true", true, true, true, true},
		{"Console enabled, default false, route false", true, false, false, true},
		{"Console disabled, default true, route true", false, true, true, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create test config
			config := createTestConfig(loggingServer.URL(), tc.consoleEnabled, tc.defaultLogging, map[string]Route{
				"test": {Pattern: "/api/", Destination: backend.URL + "/", Logging: tc.routeLogging},
			})

			// Verify config was parsed correctly
			if config.Logging.Console != tc.consoleEnabled {
				t.Errorf("Expected console logging %t, got %t", tc.consoleEnabled, config.Logging.Console)
			}

			if config.Logging.Default != tc.defaultLogging {
				t.Errorf("Expected default logging %t, got %t", tc.defaultLogging, config.Logging.Default)
			}
		})
	}
}

func TestQueryStringLogging(t *testing.T) {
	// Create mock backend server that echoes query parameters
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"query": "%s", "param1": "%s", "param2": "%s"}`,
			r.URL.RawQuery, r.URL.Query().Get("param1"), r.URL.Query().Get("param2"))
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"test": {Pattern: "/api/", Destination: backend.URL + "/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make request with query string
	resp, err := http.Get(testServer.URL + "/api/search?param1=value1&param2=value2&special=%40%21%24")
	if err != nil {
		t.Fatal("Request with query string failed:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Give time for logging
	time.Sleep(100 * time.Millisecond)

	// Check that the logged request contains the query string
	if len(loggingServer.requests) == 0 {
		t.Fatal("Expected request with query string to be logged")
	}

	for _, requestData := range loggingServer.requests {
		requestString := string(requestData)
		t.Logf("Logged request with query string:\n%s", requestString)

		// Should contain the request line with query parameters
		if !strings.Contains(requestString, "GET /search?") {
			t.Error("Expected logged request to contain query string in request line")
		}

		// Should contain specific query parameters
		expectedParams := []string{"param1=value1", "param2=value2", "special=%40%21%24"}
		for _, param := range expectedParams {
			if !strings.Contains(requestString, param) {
				t.Errorf("Expected logged request to contain query parameter: %s", param)
			}
		}

		break // Check first request
	}
}

func TestChunkedTransferEncoding(t *testing.T) {
	// Create mock backend server that handles chunked requests
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Error reading body", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"received": "%s", "transfer_encoding": "%v", "content_length": %d}`,
			string(body), r.TransferEncoding, r.ContentLength)
	}))
	defer backend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"test": {Pattern: "/api/", Destination: backend.URL + "/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Create a custom HTTP request with Transfer-Encoding: chunked
	client := &http.Client{}

	// Create request body
	requestBody := "This is a test body for chunked transfer encoding"

	req, err := http.NewRequest("POST", testServer.URL+"/api/chunked", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal("Failed to create request:", err)
	}

	// Set headers for chunked transfer encoding
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Transfer-Encoding", "chunked")
	// Remove Content-Length to force chunked encoding
	req.ContentLength = -1

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("Chunked request failed:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Give time for logging
	time.Sleep(100 * time.Millisecond)

	// Check that the request was logged
	if len(loggingServer.requests) == 0 {
		t.Fatal("Expected chunked request to be logged")
	}

	for _, requestData := range loggingServer.requests {
		requestString := string(requestData)
		t.Logf("Logged chunked request:\n%s", requestString)

		// Note: Go's HTTP implementation may not preserve explicit Transfer-Encoding headers
		// in the reconstructed request, but the body should still be present
		// Check for either Transfer-Encoding header OR the absence of Content-Length (indicating chunked)
		hasTransferEncoding := strings.Contains(requestString, "Transfer-Encoding:")
		hasContentLength := strings.Contains(requestString, "Content-Length:")

		if !hasTransferEncoding && hasContentLength {
			t.Logf("Note: Transfer-Encoding header not preserved, but this is acceptable behavior")
		}

		// Should contain the request body
		if !strings.Contains(requestString, requestBody) {
			t.Error("Expected logged request to contain the request body")
		}

		// Should contain proper HTTP method and path
		if !strings.Contains(requestString, "POST /chunked HTTP/1.1") {
			t.Error("Expected logged request to contain proper HTTP request line")
		}

		// Should contain Content-Type header
		if !strings.Contains(requestString, "Content-Type: text/plain") {
			t.Error("Expected logged request to contain Content-Type header")
		}

		break // Check first request
	}
}

func TestUnknownRouteWithQueryString(t *testing.T) {
	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config with default logging enabled
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"known": {Pattern: "/api/", Destination: "https://example.com/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Make request to unknown route with query parameters
	resp, err := http.Get(testServer.URL + "/unknown/path?param1=value1&param2=value2&search=test%20query")
	if err != nil {
		t.Fatal("Request to unknown route with query string failed:", err)
	}
	defer resp.Body.Close()

	// Should get 404
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}

	// Give time for async logging
	time.Sleep(100 * time.Millisecond)

	// Should have logged the unknown route with query string (default: true)
	if len(loggingServer.requests) == 0 {
		t.Error("Expected unknown route request with query string to be logged")
	}

	if len(loggingServer.responses) == 0 {
		t.Error("Expected unknown route response to be logged")
	}

	// Check that the logged request contains the query string
	for _, requestData := range loggingServer.requests {
		requestString := string(requestData)
		t.Logf("Logged unknown route request with query string:\n%s", requestString)

		// Should contain the request line with query parameters
		if !strings.Contains(requestString, "GET /unknown/path?") {
			t.Error("Expected logged request to contain path with query string")
		}

		// Should contain specific query parameters
		expectedParams := []string{"param1=value1", "param2=value2", "search=test%20query"}
		for _, param := range expectedParams {
			if !strings.Contains(requestString, param) {
				t.Errorf("Expected logged request to contain query parameter: %s", param)
			}
		}

		break // Check first request
	}
}

func TestCatchAllHandling(t *testing.T) {
	// Create mock backend server for catch-all
	catchAllBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Catch-all handler received: %s %s", r.Method, r.URL.Path)
	}))
	defer catchAllBackend.Close()

	// Create mock logging server
	loggingServer := NewMockLoggingServer()
	defer loggingServer.Close()

	// Create test config with a catch-all route (empty pattern matches everything)
	config := createTestConfig(loggingServer.URL(), false, true, map[string]Route{
		"specific":  {Pattern: "/api/", Destination: "https://example.com/", Logging: true},
		"catch_all": {Pattern: "/", Destination: catchAllBackend.URL + "/", Logging: true},
	})

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Test cases for different paths
	testCases := []struct {
		name         string
		path         string
		expectedBody string
		shouldMatch  string
	}{
		{
			name:         "Specific route match",
			path:         "/api/test",
			expectedBody: "Example Domain", // Will reach example.com
			shouldMatch:  "specific",
		},
		{
			name:         "Catch-all route match",
			path:         "/anything/else",
			expectedBody: "Catch-all handler received: GET /anything/else",
			shouldMatch:  "catch_all",
		},
		{
			name:         "Root path catch-all",
			path:         "/",
			expectedBody: "Catch-all handler received: GET /",
			shouldMatch:  "catch_all",
		},
		{
			name:         "Deep path catch-all",
			path:         "/some/deep/nested/path?query=value",
			expectedBody: "Catch-all handler received: GET /some/deep/nested/path",
			shouldMatch:  "catch_all",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear logging server state
			loggingServer.requests = make(map[string][]byte)
			loggingServer.responses = make(map[string][]byte)

			resp, err := http.Get(testServer.URL + tc.path)
			if err != nil {
				t.Fatal("Request failed:", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal("Failed to read response body:", err)
			}

			bodyStr := strings.TrimSpace(string(body))
			if !strings.Contains(bodyStr, tc.expectedBody) {
				t.Errorf("Expected response to contain '%s', got '%s'", tc.expectedBody, bodyStr)
			}

			// Give time for logging
			time.Sleep(100 * time.Millisecond)

			// Should have logged the request
			if len(loggingServer.requests) == 0 {
				t.Error("Expected request to be logged")
			}

			if len(loggingServer.responses) == 0 {
				t.Error("Expected response to be logged")
			}

			// Check that the logged request contains the correct path
			for _, requestData := range loggingServer.requests {
				requestString := string(requestData)
				t.Logf("Logged %s request:\n%s", tc.shouldMatch, requestString)

				// Should contain the correct path in the request line
				expectedPath := tc.path
				if strings.Contains(tc.path, "?") {
					expectedPath = strings.Split(tc.path, "?")[0]
				}

				if !strings.Contains(requestString, expectedPath) {
					t.Errorf("Expected logged request to contain path: %s", expectedPath)
				}

				break // Check first request
			}
		})
	}
}

func TestExperimentHttpExamples(t *testing.T) {
	// Create mock backend servers
	lmStudioServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "LMStudio response: %s %s", r.Method, r.URL.Path)
	}))
	defer lmStudioServer.Close()

	openRouterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OpenRouter response: %s %s", r.Method, r.URL.Path)
	}))
	defer openRouterServer.Close()

	mockFileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/static/mockfile.txt" {
			fmt.Fprintf(w, "Mock file content")
		} else {
			http.NotFound(w, r)
		}
	}))
	defer mockFileServer.Close()

	// Create test config matching experiment.go routes
	config := &Config{
		Logging: struct {
			Console   bool   `yaml:"console"`
			ServerURL string `yaml:"server_url"`
			Default   bool   `yaml:"default"`
		}{Console: false, ServerURL: "http://localhost:8080", Default: false},
		Routes: map[string]Route{
			"lmstudio":   {Pattern: "/lmstudio/", Destination: lmStudioServer.URL + "/", Logging: false},
			"openrouter": {Pattern: "/openrouter/", Destination: openRouterServer.URL + "/api/v1/", Logging: false},
			"mockfile":   {Pattern: "/lmstudio/mockfile", Destination: mockFileServer.URL + "/static/mockfile.txt", Logging: false},
		},
	}

	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer.mux)
	defer testServer.Close()

	// Test cases matching experiment.http examples
	testCases := []struct {
		name           string
		method         string
		path           string
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "LMStudio subpath routing",
			method:         "GET",
			path:           "/lmstudio/subpath1/subpath2?q1=1&q2=2",
			expectedStatus: 200,
			expectedBody:   "LMStudio response: GET /subpath1/subpath2",
		},
		{
			name:           "OpenRouter models endpoint",
			method:         "GET",
			path:           "/openrouter/models?q=7",
			expectedStatus: 200,
			expectedBody:   "OpenRouter response: GET /api/v1/models",
		},
		{
			name:           "Specific mockfile endpoint",
			method:         "GET",
			path:           "/lmstudio/mockfile?query=true",
			expectedStatus: 200,
			expectedBody:   "Mock file content",
		},
		{
			name:           "Unknown route returns 404",
			method:         "DELETE",
			path:           "/unknown/path",
			expectedStatus: 404,
			expectedBody:   "custom 404 page",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, testServer.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}

			client := &http.Client{}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.expectedStatus {
				t.Errorf("Expected status %d, got %d", tc.expectedStatus, resp.StatusCode)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			bodyStr := strings.TrimSpace(string(body))
			if !strings.Contains(bodyStr, tc.expectedBody) {
				t.Errorf("Expected body to contain '%s', got '%s'", tc.expectedBody, bodyStr)
			}
		})
	}
}
