package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Mock server for testing
type MockServer struct {
	server *httptest.Server
	routes map[string]http.HandlerFunc
}

func NewMockServer() *MockServer {
	mock := &MockServer{
		routes: make(map[string]http.HandlerFunc),
	}
	
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if handler, exists := mock.routes[r.URL.Path]; exists {
			handler(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	
	mock.server = httptest.NewServer(mux)
	return mock
}

func (m *MockServer) Close() {
	m.server.Close()
}

func (m *MockServer) URL() string {
	return m.server.URL
}

func (m *MockServer) AddRoute(path string, handler http.HandlerFunc) {
	m.routes[path] = handler
}

func TestConfigLoading(t *testing.T) {
	// Create test config
	configData := `
server:
  port: 8080
  host: "localhost"

logging:
  console: true
  file: "test.log"
  binary_files: true

routes:
  - source: "/api/v1/"
    destination: "https://example.com/api/v1/"
    name: "test"
`
	
	// Write test config
	err := os.WriteFile("test_config.yaml", []byte(configData), 0666)
	if err != nil {
		t.Fatal("Failed to write test config:", err)
	}
	defer os.Remove("test_config.yaml")
	
	// Load config
	config, err := loadConfig("test_config.yaml")
	if err != nil {
		t.Fatal("Failed to load config:", err)
	}
	
	// Verify config values
	if config.Server.Port != 8080 {
		t.Errorf("Expected port 8080, got %d", config.Server.Port)
	}
	
	if config.Server.Host != "localhost" {
		t.Errorf("Expected host 'localhost', got '%s'", config.Server.Host)
	}
	
	if !config.Logging.Console {
		t.Error("Expected logging.console to be true")
	}
	
	if config.Logging.File != "test.log" {
		t.Errorf("Expected log file 'test.log', got '%s'", config.Logging.File)
	}
	
	if len(config.Routes) != 1 {
		t.Errorf("Expected 1 route, got %d", len(config.Routes))
	}
	
	route := config.Routes[0]
	if route.Source != "/api/v1/" {
		t.Errorf("Expected source '/api/v1/', got '%s'", route.Source)
	}
	
	if route.Destination != "https://example.com/api/v1/" {
		t.Errorf("Expected destination 'https://example.com/api/v1/', got '%s'", route.Destination)
	}
	
	if route.Name != "test" {
		t.Errorf("Expected name 'test', got '%s'", route.Name)
	}
}

func TestBasicProxying(t *testing.T) {
	// Create mock backend server
	mockServer := NewMockServer()
	defer mockServer.Close()
	
	// Add test route to mock server - the path will be /test after proxy transformation
	mockServer.AddRoute("/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		body, _ := io.ReadAll(r.Body)
		response := map[string]interface{}{
			"echo": string(body),
			"method": r.Method,
			"headers": r.Header,
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
	
	// Create test config
	config := &Config{
		Server: struct {
			Port int    `yaml:"port"`
			Host string `yaml:"host"`
		}{Port: 0, Host: "localhost"}, // Port 0 for auto-assignment
		Logging: struct {
			Console     bool   `yaml:"console"`
			File        string `yaml:"file"`
			BinaryFiles bool   `yaml:"binary_files"`
		}{Console: false, File: "", BinaryFiles: false},
		Routes: []Route{
			{
				Source:      "/api/v1/",
				Destination: mockServer.URL() + "/",
				Name:        "test",
			},
		},
	}
	
	// Create proxy server
	proxyServer := NewProxyServer(config)
	
	// Create test server for proxy
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Find matching route - match longer paths first
		var matchedHandler *RouteHandler
		var matchedLength int
		
		for sourcePath, handler := range proxyServer.routes {
			trimmedSource := strings.TrimSuffix(sourcePath, "/")
			if strings.HasPrefix(r.URL.Path, trimmedSource) && len(trimmedSource) > matchedLength {
				matchedHandler = handler
				matchedLength = len(trimmedSource)
			}
		}
		
		if matchedHandler != nil {
			matchedHandler.ServeHTTP(w, r)
			return
		}
		
		http.NotFound(w, r)
	}))
	defer testServer.Close()
	
	// Test request
	requestBody := `{"test": "data"}`
	resp, err := http.Post(testServer.URL+"/api/v1/test", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal("Request failed:", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
		return
	}
	
	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		t.Fatal("Failed to decode response:", err)
	}
	
	if response["echo"] != requestBody {
		t.Errorf("Expected echo '%s', got '%v'", requestBody, response["echo"])
	}
	
	if response["method"] != "POST" {
		t.Errorf("Expected method 'POST', got '%v'", response["method"])
	}
}

func TestStreamingResponse(t *testing.T) {
	// Create mock streaming server
	mockServer := NewMockServer()
	defer mockServer.Close()
	
	mockServer.AddRoute("/stream", func(w http.ResponseWriter, r *http.Request) {
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
			"data: {\"chunk\": 1, \"text\": \"Hello\"}\n\n",
			"data: {\"chunk\": 2, \"text\": \" world\"}\n\n",
			"data: {\"chunk\": 3, \"text\": \"!\"}\n\n",
			"data: [DONE]\n\n",
		}
		
		for _, event := range events {
			fmt.Fprint(w, event)
			flusher.Flush()
			time.Sleep(10 * time.Millisecond) // Simulate streaming delay
		}
	})
	
	// Create test config
	config := &Config{
		Server: struct {
			Port int    `yaml:"port"`
			Host string `yaml:"host"`
		}{Port: 0, Host: "localhost"},
		Logging: struct {
			Console     bool   `yaml:"console"`
			File        string `yaml:"file"`
			BinaryFiles bool   `yaml:"binary_files"`
		}{Console: false, File: "test_stream.log", BinaryFiles: true},
		Routes: []Route{
			{
				Source:      "/api/v1/",
				Destination: mockServer.URL() + "/",
				Name:        "streaming_test",
			},
		},
	}
	
	// Create proxy server
	proxyServer := NewProxyServer(config)
	
	// Create test server for proxy
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for sourcePath, handler := range proxyServer.routes {
			if strings.HasPrefix(r.URL.Path, strings.TrimSuffix(sourcePath, "/")) {
				handler.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer testServer.Close()
	
	// Test streaming request
	resp, err := http.Get(testServer.URL + "/api/v1/stream")
	if err != nil {
		t.Fatal("Streaming request failed:", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/event-stream") {
		t.Errorf("Expected SSE content type, got '%s'", contentType)
	}
	
	// Read the streaming response
	var buffer bytes.Buffer
	_, err = io.Copy(&buffer, resp.Body)
	if err != nil {
		t.Fatal("Failed to read streaming response:", err)
	}
	
	responseData := buffer.String()
	
	// Verify we got all the expected chunks
	expectedChunks := []string{
		"data: {\"chunk\": 1, \"text\": \"Hello\"}",
		"data: {\"chunk\": 2, \"text\": \" world\"}",
		"data: {\"chunk\": 3, \"text\": \"!\"}",
		"data: [DONE]",
	}
	
	for _, expectedChunk := range expectedChunks {
		if !strings.Contains(responseData, expectedChunk) {
			t.Errorf("Expected chunk '%s' not found in response", expectedChunk)
		}
	}
	
	// Clean up log files
	os.Remove("test_stream.log")
	
	// Clean up any binary files created during test
	files, _ := os.ReadDir(".")
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".bin") {
			os.Remove(file.Name())
		}
	}
}

func TestMultipleRoutes(t *testing.T) {
	// Create two mock servers
	mockServer1 := NewMockServer()
	defer mockServer1.Close()
	
	mockServer2 := NewMockServer()
	defer mockServer2.Close()
	
	// Add routes to mock servers
	mockServer1.AddRoute("/openrouter", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"service": "openrouter", "path": r.URL.Path})
	})
	
	mockServer2.AddRoute("/openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"service": "openai", "path": r.URL.Path})
	})
	
	// Create test config with multiple routes
	config := &Config{
		Server: struct {
			Port int    `yaml:"port"`
			Host string `yaml:"host"`
		}{Port: 0, Host: "localhost"},
		Logging: struct {
			Console     bool   `yaml:"console"`
			File        string `yaml:"file"`
			BinaryFiles bool   `yaml:"binary_files"`
		}{Console: false, File: "", BinaryFiles: false},
		Routes: []Route{
			{
				Source:      "/api/v1/",
				Destination: mockServer1.URL() + "/",
				Name:        "openrouter",
			},
			{
				Source:      "/v1/",
				Destination: mockServer2.URL() + "/",
				Name:        "openai",
			},
		},
	}
	
	// Create proxy server
	proxyServer := NewProxyServer(config)
	
	// Create test server for proxy
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for sourcePath, handler := range proxyServer.routes {
			if strings.HasPrefix(r.URL.Path, strings.TrimSuffix(sourcePath, "/")) {
				handler.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer testServer.Close()
	
	// Test first route
	resp1, err := http.Get(testServer.URL + "/api/v1/openrouter")
	if err != nil {
		t.Fatal("First route request failed:", err)
	}
	defer resp1.Body.Close()
	
	var response1 map[string]string
	json.NewDecoder(resp1.Body).Decode(&response1)
	
	if response1["service"] != "openrouter" {
		t.Errorf("Expected service 'openrouter', got '%s'", response1["service"])
	}
	
	// Test second route
	resp2, err := http.Get(testServer.URL + "/v1/openai")
	if err != nil {
		t.Fatal("Second route request failed:", err)
	}
	defer resp2.Body.Close()
	
	var response2 map[string]string
	json.NewDecoder(resp2.Body).Decode(&response2)
	
	if response2["service"] != "openai" {
		t.Errorf("Expected service 'openai', got '%s'", response2["service"])
	}
}

func TestFilenameGeneration(t *testing.T) {
	now := time.Now()
	
	// Test basic filename generation
	filename1 := generateUniqueFilename(now)
	filename2 := generateUniqueFilename(now.Add(time.Nanosecond))
	
	if filename1 == filename2 {
		t.Error("Expected different filenames for different timestamps")
	}
	
	// Test that filenames have expected format
	expectedPrefix := now.Format("2006-01-02_15-04-05")
	if !strings.HasPrefix(filename1, expectedPrefix) {
		t.Errorf("Expected filename to start with '%s', got '%s'", expectedPrefix, filename1)
	}
	
	// Test collision prevention
	// Create a fake file to trigger collision
	testFilename := generateUniqueFilename(now)
	fakeFile := testFilename + "-request.bin"
	os.WriteFile(fakeFile, []byte("test"), 0666)
	defer os.Remove(fakeFile)
	
	// Generate new filename - should be different due to collision
	newFilename := generateUniqueFilename(now)
	if newFilename == testFilename {
		t.Error("Expected different filename due to collision, but got the same")
	}
	
	if !strings.Contains(newFilename, "_1") {
		t.Error("Expected collision-resolved filename to contain '_1'")
	}
}