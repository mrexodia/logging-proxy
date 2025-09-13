package loggingproxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

func TestFileLogging(t *testing.T) {
	// Create a temporary directory for logging
	logDir := "test_logs"
	os.RemoveAll(logDir)       // Clean up any existing logs
	defer os.RemoveAll(logDir) // Clean up after test

	// Create file logger
	fileLogger, err := NewFileLogger(logDir)
	if err != nil {
		t.Fatalf("Failed to create file logger: %v", err)
	}

	// Create mock backend server
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"message": "Hello from backend", "path": "` + r.URL.Path + `"}`))
	}))
	defer backend.Close()

	// Create proxy server with file logging
	server := NewProxyServer("")
	err = server.AddRoute("/api/", backend.URL+"/", fileLogger)
	if err != nil {
		t.Fatalf("Failed to add route: %v", err)
	}

	// Create test server for proxy
	testServer := httptest.NewServer(server)
	defer testServer.Close()

	// Make a test request
	requestBody := `{"test": "data"}`
	resp, err := http.Post(testServer.URL+"/api/test", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// Give logging goroutines time to complete
	time.Sleep(100 * time.Millisecond)

	// Check if files were created
	files, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("Failed to read log directory: %v", err)
	}

	t.Logf("Files created in %s:", logDir)
	for _, file := range files {
		t.Logf("- %s", file.Name())
	}

	// Verify we have both request and response files + metadata
	requestFiles := 0
	responseFiles := 0
	metadataFiles := 0

	for _, file := range files {
		if strings.Contains(file.Name(), "request.bin") {
			requestFiles++
		}
		if strings.Contains(file.Name(), "response.bin") {
			responseFiles++
		}
		if strings.Contains(file.Name(), "metadata.json") {
			metadataFiles++
		}
	}

	t.Logf("Summary:")
	t.Logf("- Request .bin files: %d", requestFiles)
	t.Logf("- Response .bin files: %d", responseFiles)
	t.Logf("- Metadata .json files: %d", metadataFiles)

	// Verify all expected files were created
	if requestFiles != 1 {
		t.Errorf("Expected 1 request file, got %d", requestFiles)
	}
	if responseFiles != 1 {
		t.Errorf("Expected 1 response file, got %d", responseFiles)
	}
	if metadataFiles != 2 {
		t.Errorf("Expected 2 metadata files, got %d", metadataFiles)
	}

	// Verify the files have content
	for _, file := range files {
		info, err := file.Info()
		if err != nil {
			t.Errorf("Failed to get file info for %s: %v", file.Name(), err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("File %s is empty", file.Name())
		}
	}

	// Delete the files to make sure no handles are left open
	for _, file := range files {
		filePath := path.Join(logDir, file.Name())
		err := os.Remove(filePath)
		if err != nil {
			t.Errorf("Failed to delete file %s: %v", file.Name(), err)
		}
	}
}
