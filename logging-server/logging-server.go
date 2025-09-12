package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	http.HandleFunc("/", handleLogRequest)
	fmt.Println("Logging server starting on :8080")
	fmt.Println("Saving logs to ./logs/ directory")
	
	// Ensure logs directory exists
	os.MkdirAll("logs", 0755)
	
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleLogRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PUT" {
		http.Error(w, "Only PUT method allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract request ID and stream type from URL path
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) != 2 {
		http.Error(w, "Invalid path format. Expected /{requestID}/{request|response}", http.StatusBadRequest)
		return
	}

	requestID := pathParts[0]
	streamType := pathParts[1]

	if streamType != "request" && streamType != "response" {
		http.Error(w, "Stream type must be 'request' or 'response'", http.StatusBadRequest)
		return
	}

	// Create filename with timestamp
	timestamp := time.Now().Format("2006-01-02_15-04-05.000")
	filename := fmt.Sprintf("logs/%s_%s_%s.bin", timestamp, requestID[:8], streamType)

	// Create the file
	file, err := os.Create(filename)
	if err != nil {
		log.Printf("Error creating file %s: %v", filename, err)
		http.Error(w, "Failed to create log file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Copy the request body to the file
	bytesWritten, err := io.Copy(file, r.Body)
	if err != nil {
		log.Printf("Error writing to file %s: %v", filename, err)
		http.Error(w, "Failed to write log data", http.StatusInternalServerError)
		return
	}

	log.Printf("Saved %s for request %s (%d bytes) -> %s", 
		streamType, requestID[:8], bytesWritten, filepath.Base(filename))

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "Logged %d bytes to %s\n", bytesWritten, filename)
}