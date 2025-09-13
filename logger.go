package loggingproxy

import (
	"io"
	"net/url"
	"time"
)

// RequestMetadata contains information about a request for logging
type RequestMetadata struct {
	ID             string  `json:"id"`
	Pattern        string  `json:"pattern"`
	Method         string  `json:"method"`
	SourceURL      url.URL `json:"source_url"`
	DestinationURL url.URL `json:"target_url"`
}

// Logger interface for dependency injection of logging functionality
type Logger interface {
	// LogRequest logs a request with its metadata and raw HTTP stream
	LogRequest(metadata RequestMetadata, timestamp time.Time, rawRequestStream io.ReadCloser) error

	// LogResponse logs a response with its metadata and raw HTTP stream
	LogResponse(metadata RequestMetadata, timestamp time.Time, rawResponseStream io.ReadCloser) error
}

// NoOpLogger is a logger that does nothing (for when logging is disabled)
type NoOpLogger struct{}

func (n *NoOpLogger) LogRequest(metadata RequestMetadata, timestamp time.Time, rawRequestStream io.ReadCloser) error {
	// Must consume the stream to avoid blocking the TeeReader
	defer rawRequestStream.Close()
	io.Copy(io.Discard, rawRequestStream)
	return nil
}

func (n *NoOpLogger) LogResponse(metadata RequestMetadata, timestamp time.Time, rawResponseStream io.ReadCloser) error {
	// Must consume the stream to avoid blocking the TeeReader
	defer rawResponseStream.Close()
	io.Copy(io.Discard, rawResponseStream)
	return nil
}
