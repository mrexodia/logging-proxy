package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

	// Create test config
	configContent := fmt.Sprintf(`
server:
  port: 0
  host: "localhost"

logging:
  console: false
  server_url: "%s"

routes:
  test:
    source: "/api/v1/"
    destination: "%s/"
    logging: true
`, loggingServer.URL(), backend.URL)

	err := os.WriteFile("test_config_new.yaml", []byte(configContent), 0666)
	if err != nil {
		t.Fatal("Failed to write test config:", err)
	}
	defer os.Remove("test_config_new.yaml")

	// Load config
	config, err := loadConfig("test_config_new.yaml")
	if err != nil {
		t.Fatal("Failed to load config:", err)
	}

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(http.HandlerFunc(proxyServer.handleRequest))
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

	// Create test config
	configContent := fmt.Sprintf(`
server:
  port: 0
  host: "localhost"

logging:
  console: false
  server_url: "%s"

routes:
  streaming_test:
    source: "/api/v1/"
    destination: "%s/"
    logging: true
`, loggingServer.URL(), backend.URL)

	err := os.WriteFile("test_streaming_config.yaml", []byte(configContent), 0666)
	if err != nil {
		t.Fatal("Failed to write test config:", err)
	}
	defer os.Remove("test_streaming_config.yaml")

	// Load config
	config, err := loadConfig("test_streaming_config.yaml")
	if err != nil {
		t.Fatal("Failed to load config:", err)
	}

	// Create proxy server
	proxyServer := NewProxyServer(config)

	// Create test server for proxy
	testServer := httptest.NewServer(http.HandlerFunc(proxyServer.handleRequest))
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
		}{Console: true, ServerURL: "http://localhost:8080"},
		Routes: map[string]Route{
			"test": {Source: "/api/v1/", Destination: "https://example.com/", Logging: true},
		},
	}

	server := NewProxyServer(config)

	if len(server.routes) != 1 {
		t.Errorf("Expected 1 route, got %d", len(server.routes))
	}

	route := server.routes["/api/v1/"]
	if route == nil {
		t.Error("Route not found")
	}

	if route.Destination != "https://example.com/" {
		t.Errorf("Expected destination 'https://example.com/', got '%s'", route.Destination)
	}

	if !route.Logging {
		t.Error("Expected logging to be enabled for route")
	}
}
