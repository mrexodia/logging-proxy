package loggingproxy

import (
	"io"
	"net/http"
)

// RequestMetadata contains information about a request for logging
type RequestMetadata struct {
	ID        string `json:"id"`
	Pattern   string `json:"pattern"`
	Method    string `json:"method"`
	SourceURL string `json:"source_url"`
	TargetURL string `json:"target_url"`
}

// Logger interface for dependency injection of logging functionality
type Logger interface {
	// LogRequest logs a request with its metadata and body stream
	LogRequest(metadata RequestMetadata, requestStream io.ReadCloser, request *http.Request) error

	// LogResponse logs a response with its metadata and body stream
	LogResponse(metadata RequestMetadata, responseStream io.ReadCloser, response *http.Response) error
}

// NoOpLogger is a logger that does nothing (for when logging is disabled)
type NoOpLogger struct{}

func (n *NoOpLogger) LogRequest(metadata RequestMetadata, requestStream io.ReadCloser, request *http.Request) error {
	// Must consume the stream to avoid blocking the TeeReader
	defer requestStream.Close()
	io.Copy(io.Discard, requestStream)
	return nil
}

func (n *NoOpLogger) LogResponse(metadata RequestMetadata, responseStream io.ReadCloser, response *http.Response) error {
	// Must consume the stream to avoid blocking the TeeReader
	defer responseStream.Close()
	io.Copy(io.Discard, responseStream)
	return nil
}
