package loggingproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileLogger implements the Logger interface and writes logs to files
type FileLogger struct {
	LogDir string
}

// NewFileLogger creates a new file-based logger
func NewFileLogger(logDir string) (*FileLogger, error) {
	// Ensure log directory exists
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	return &FileLogger{
		LogDir: logDir,
	}, nil
}

// LogRequest logs a request with its metadata and body stream to a file
func (f *FileLogger) LogRequest(metadata RequestMetadata, requestStream io.ReadCloser, request *http.Request) error {
	return f.logStream(metadata, requestStream, "request", request, nil)
}

// LogResponse logs a response with its metadata and body stream to a file
func (f *FileLogger) LogResponse(metadata RequestMetadata, responseStream io.ReadCloser, response *http.Response) error {
	return f.logStream(metadata, responseStream, "response", nil, response)
}

// logStream handles the common logic for logging request/response streams
func (f *FileLogger) logStream(metadata RequestMetadata, stream io.ReadCloser, streamType string, request *http.Request, response *http.Response) error {
	defer stream.Close()
	timestamp := time.Now().Format("2006-01-02_15-04-05.000")
	filename := fmt.Sprintf("%s_%s_%s.bin", timestamp, metadata.ID[:8], streamType)
	filePath := filepath.Join(f.LogDir, filename)

	// Create the log file
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create log file %s: %w", filePath, err)
	}
	defer file.Close()

	// Write HTTP headers and body
	bytesWritten, err := f.writeHTTPStream(file, streamType, request, response, stream, "")
	if err != nil {
		return fmt.Errorf("failed to write HTTP stream: %w", err)
	}

	// Create and save metadata
	logMetadata := FileLogMetadata{
		StreamType:    streamType,
		Metadata:      metadata,
		Timestamp:     time.Now(),
		Filename:      filename,
		ContentLength: bytesWritten,
	}

	metadataFilename := fmt.Sprintf("%s_%s_%s_metadata.json", timestamp, metadata.ID[:8], streamType)
	metadataPath := filepath.Join(f.LogDir, metadataFilename)

	if err := f.saveMetadata(logMetadata, metadataPath); err != nil {
		log.Printf("Warning: Failed to save metadata %s: %v", metadataPath, err)
	}

	log.Printf("Saved %s for request %s (%d bytes) -> %s", streamType, metadata.ID[:8], bytesWritten, filename)
	return nil
}

// writeHTTPStream writes the HTTP request/response with headers and body to the file
func (f *FileLogger) writeHTTPStream(writer io.Writer, streamType string, request *http.Request, response *http.Response, bodyStream io.Reader, proxyPath string) (int64, error) {
	var totalBytes int64

	if streamType == "request" && request != nil {
		// Write request line
		requestURI := request.URL.Path
		if request.URL.RawQuery != "" {
			requestURI += "?" + request.URL.RawQuery
		}
		n, err := fmt.Fprintf(writer, "%s %s %s\r\n", request.Method, requestURI, request.Proto)
		if err != nil {
			return totalBytes, err
		}
		totalBytes += int64(n)

		// Write Host header
		if request.Host != "" {
			n, err := fmt.Fprintf(writer, "Host: %s\r\n", request.Host)
			if err != nil {
				return totalBytes, err
			}
			totalBytes += int64(n)
		}

		// Write other headers (skip special ones)
		for name, values := range request.Header {
			lowerName := strings.ToLower(name)
			if lowerName != "host" && lowerName != "content-length" && lowerName != "transfer-encoding" {
				for _, value := range values {
					n, err := fmt.Fprintf(writer, "%s: %s\r\n", name, value)
					if err != nil {
						return totalBytes, err
					}
					totalBytes += int64(n)
				}
			}
		}

		// Add custom proxy path header
		if proxyPath != "" {
			n, err := fmt.Fprintf(writer, "X-Proxy-Path: %s\r\n", proxyPath)
			if err != nil {
				return totalBytes, err
			}
			totalBytes += int64(n)
		}

	} else if streamType == "response" && response != nil {
		// Write response status line
		statusText := http.StatusText(response.StatusCode)
		n, err := fmt.Fprintf(writer, "%s %d %s\r\n", response.Proto, response.StatusCode, statusText)
		if err != nil {
			return totalBytes, err
		}
		totalBytes += int64(n)

		// Write response headers
		for name, values := range response.Header {
			for _, value := range values {
				n, err := fmt.Fprintf(writer, "%s: %s\r\n", name, value)
				if err != nil {
					return totalBytes, err
				}
				totalBytes += int64(n)
			}
		}
	}

	// Write separator between headers and body
	n, err := fmt.Fprintf(writer, "\r\n")
	if err != nil {
		return totalBytes, err
	}
	totalBytes += int64(n)

	// Write body
	copied, err := io.Copy(writer, bodyStream)
	if err != nil {
		return totalBytes, err
	}
	totalBytes += copied

	return totalBytes, nil
}

// FileLogMetadata represents metadata for file-based logging
type FileLogMetadata struct {
	StreamType    string          `json:"stream_type"`
	Metadata      RequestMetadata `json:"metadata"`
	Timestamp     time.Time       `json:"timestamp"`
	Filename      string          `json:"filename"`
	ContentLength int64           `json:"content_length"`
}

// saveMetadata saves the metadata to a JSON file
func (f *FileLogger) saveMetadata(metadata FileLogMetadata, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(metadata)
}
