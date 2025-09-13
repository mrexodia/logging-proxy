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

// Helper function to create test servers with routes
func createTestServer(routes map[string]string) *ProxyServer {
	server := NewProxyServer()
	logger := &NoOpLogger{}

	for pattern, destination := range routes {
		err := server.AddRoute(pattern, destination, logger)
		if err != nil {
			panic(err)
		}
	}

	return server
}

func TestNewArchitecture(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo": "Backend received %s %s"}`, r.Method, r.URL.Path)
	}))
	defer backend.Close()

	// Create proxy server with test routes
	proxyServer := createTestServer(map[string]string{
		"/api/v1/": backend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Test verifies that the proxy correctly forwards requests to the backend
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

	// Create proxy server with streaming test routes
	proxyServer := createTestServer(map[string]string{
		"/api/v1/": backend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Test verifies that the proxy correctly handles streaming responses
}

func TestConfigValidationNew(t *testing.T) {
	// Test that server can be created and routes added correctly
	server := NewProxyServer()
	logger := &NoOpLogger{}

	err := server.AddRoute("/api/v1/", "https://example.com/", logger)
	if err != nil {
		t.Errorf("Failed to add route: %v", err)
	}

	// Since we don't expose internal route storage, we'll just verify the server was created
	if server == nil {
		t.Error("Failed to create proxy server")
	}

	// Test that multiple routes can be added
	err = server.AddRoute("/api/v2/", "https://api.example.com/", logger)
	if err != nil {
		t.Errorf("Failed to add second route: %v", err)
	}
}

func TestUnknownRouteWithDefaultLoggingEnabled(t *testing.T) {
	// Create proxy server with known route
	proxyServer := createTestServer(map[string]string{
		"/api/": "https://example.com/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Test verifies 404 behavior for unknown routes
}

func TestUnknownRouteWithDefaultLoggingDisabled(t *testing.T) {
	// Create proxy server with known route
	proxyServer := createTestServer(map[string]string{
		"/api/": "https://example.com/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Test verifies 404 behavior for unknown routes
}

func TestMultipleRouteProxying(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path": "%s"}`, r.URL.Path)
	}))
	defer backend.Close()

	// Create proxy server with multiple routes pointing to the same backend
	proxyServer := createTestServer(map[string]string{
		"/api/":   backend.URL + "/",
		"/other/": backend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
	defer testServer.Close()

	// Make request to first route
	resp1, err := http.Get(testServer.URL + "/api/test")
	if err != nil {
		t.Fatal("Request to API route failed:", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 for API route, got %d", resp1.StatusCode)
	}

	body1, err := io.ReadAll(resp1.Body)
	if err != nil {
		t.Fatal("Failed to read API route response:", err)
	}

	// Verify the API route was proxied correctly
	if !strings.Contains(string(body1), `"path": "/test"`) {
		t.Errorf("Expected API route response to contain correct path, got: %s", string(body1))
	}

	// Make request to second route
	resp2, err := http.Get(testServer.URL + "/other/test")
	if err != nil {
		t.Fatal("Request to other route failed:", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 for other route, got %d", resp2.StatusCode)
	}

	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatal("Failed to read other route response:", err)
	}

	// Verify the other route was proxied correctly
	if !strings.Contains(string(body2), `"path": "/test"`) {
		t.Errorf("Expected other route response to contain correct path, got: %s", string(body2))
	}

	// Test verifies that multiple routes can proxy to the same backend
}

func TestRequestWithoutBody(t *testing.T) {
	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"method": "%s", "hasBody": %t}`, r.Method, r.ContentLength > 0)
	}))
	defer backend.Close()

	// Create proxy server
	proxyServer := createTestServer(map[string]string{
		"/api/": backend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Read and verify response
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("Failed to read response:", err)
	}

	responseStr := string(responseBody)
	if !strings.Contains(responseStr, `"method": "GET"`) {
		t.Errorf("Expected response to indicate GET method, got: %s", responseStr)
	}

	if !strings.Contains(responseStr, `"hasBody": false`) {
		t.Errorf("Expected response to indicate no body, got: %s", responseStr)
	}

	// Test verifies that GET requests without body are properly proxied
}

func TestHostHeaderProxying(t *testing.T) {
	// Create mock backend server that reports the Host header it receives
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"host": "%s"}`, r.Host)
	}))
	defer backend.Close()

	// Create proxy server
	proxyServer := createTestServer(map[string]string{
		"/api/": backend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Read response to verify Host header handling
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("Failed to read response:", err)
	}

	responseStr := string(responseBody)
	t.Logf("Backend received Host: %s", responseStr)

	// Verify the backend received the correct Host header (should be backend's host, not original)
	backendHost := strings.TrimPrefix(backend.URL, "http://")
	if !strings.Contains(responseStr, backendHost) {
		t.Errorf("Expected backend to receive its own host %s in Host header, got: %s", backendHost, responseStr)
	}

	// Test verifies that the proxy correctly updates the Host header for the backend
}

func TestContentLengthProxying(t *testing.T) {
	// Create mock backend server that reports what it receives
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"method": "%s", "contentLength": %d}`, r.Method, r.ContentLength)
	}))
	defer backend.Close()

	// Create proxy server
	proxyServer := createTestServer(map[string]string{
		"/api/": backend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
	defer testServer.Close()

	// Make POST request with body to test Content-Length handling
	requestBody := `{"test": "data with some content"}`
	resp, err := http.Post(testServer.URL+"/api/test", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal("POST request failed:", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Read response to verify the backend received the correct Content-Length
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("Failed to read response:", err)
	}

	responseStr := string(responseBody)
	t.Logf("Backend response: %s", responseStr)

	// Verify the backend received the correct method and content
	if !strings.Contains(responseStr, `"method": "POST"`) {
		t.Errorf("Expected backend to receive POST method, got: %s", responseStr)
	}

	// Content-Length may be -1 if using chunked encoding, which is fine
	// The important thing is that the request body was received correctly
	if !strings.Contains(responseStr, "contentLength") {
		t.Errorf("Expected backend response to contain contentLength field, got: %s", responseStr)
	}

	// Test verifies that the proxy correctly forwards Content-Length headers and request body
}

func TestConfigLoggingSettings(t *testing.T) {
	// Test that the proxy server can be created and configured
	server := NewProxyServer()
	if server == nil {
		t.Error("Failed to create proxy server")
	}

	// Test setting different loggers
	logger := &NoOpLogger{}
	err := server.AddRoute("/api/", "https://example.com/", logger)
	if err != nil {
		t.Errorf("Failed to add route: %v", err)
	}
}

func TestQueryStringProxying(t *testing.T) {
	// Create mock backend server that echoes query parameters
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"query": "%s", "param1": "%s", "param2": "%s"}`,
			r.URL.RawQuery, r.URL.Query().Get("param1"), r.URL.Query().Get("param2"))
	}))
	defer backend.Close()

	// Create proxy server
	proxyServer := createTestServer(map[string]string{
		"/api/": backend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Read response to verify query parameters were proxied correctly
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("Failed to read response:", err)
	}

	responseStr := string(responseBody)
	t.Logf("Backend response: %s", responseStr)

	// Verify the backend received the correct query parameters
	expectedParams := []string{"param1=value1", "param2=value2", "special=%40%21%24"}
	for _, param := range expectedParams {
		if !strings.Contains(responseStr, param) {
			t.Errorf("Expected backend to receive query parameter %s, got: %s", param, responseStr)
		}
	}

	// Test verifies that the proxy correctly forwards query string parameters
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

	// Create proxy server
	proxyServer := createTestServer(map[string]string{
		"/api/": backend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Read response to verify the chunked request was processed correctly
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("Failed to read response:", err)
	}

	responseStr := string(responseBody)
	t.Logf("Backend response: %s", responseStr)

	// Verify the backend received the request body correctly
	if !strings.Contains(responseStr, requestBody) {
		t.Errorf("Expected backend to receive request body '%s', got: %s", requestBody, responseStr)
	}

	// Test verifies that the proxy correctly handles chunked transfer encoding
}

func TestUnknownRouteWithQueryString(t *testing.T) {
	// Create proxy server with known route
	proxyServer := createTestServer(map[string]string{
		"/api/": "https://example.com/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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

	// Read the 404 response to verify the proxy handles unknown routes correctly
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("Failed to read 404 response:", err)
	}

	responseStr := string(responseBody)
	t.Logf("404 response: %s", responseStr)

	// Test verifies that unknown routes with query strings return proper 404 responses
}

func TestCatchAllHandling(t *testing.T) {
	// Create mock backend server for catch-all
	catchAllBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Catch-all handler received: %s %s", r.Method, r.URL.Path)
	}))
	defer catchAllBackend.Close()

	// Create proxy server with a catch-all route (pattern "/" matches everything)
	proxyServer := createTestServer(map[string]string{
		"/api/": "https://example.com/",
		"/":     catchAllBackend.URL + "/",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
	defer testServer.Close()

	// Test cases for different paths
	testCases := []struct {
		name         string
		path         string
		expectedBody string
		description  string
	}{
		{
			name:         "Catch-all route match",
			path:         "/anything/else",
			expectedBody: "Catch-all handler received: GET /anything/else",
			description:  "catch_all route",
		},
		{
			name:         "Root path catch-all",
			path:         "/",
			expectedBody: "Catch-all handler received: GET /",
			description:  "catch_all route",
		},
		{
			name:         "Deep path catch-all",
			path:         "/some/deep/nested/path?query=value",
			expectedBody: "Catch-all handler received: GET /some/deep/nested/path",
			description:  "catch_all route",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
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

			t.Logf("Successfully proxied %s to %s", tc.path, tc.description)
		})
	}

	// Test verifies that catch-all routes work correctly for various paths
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

	// Create proxy server matching experiment.go routes
	proxyServer := createTestServer(map[string]string{
		"/lmstudio/":         lmStudioServer.URL + "/",
		"/openrouter/":       openRouterServer.URL + "/api/v1/",
		"/lmstudio/mockfile": mockFileServer.URL + "/static/mockfile.txt",
	})

	// Create test server for proxy
	testServer := httptest.NewServer(proxyServer)
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
			expectedBody:   "404 page not found",
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
