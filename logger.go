package loggingproxy

import (
	"io"
	"time"
)

// RequestMetadata contains information about a request for logging
type RequestMetadata struct {
	ID                       string     `json:"id"`
	Pattern                  string     `json:"pattern"`
	Method                   string     `json:"method"`
	SourceURL                string     `json:"source_url"`
	DestinationURL           string     `json:"target_url"`
	RequestStartedAt         time.Time  `json:"request_started_at"`
	UpstreamResponseAt       *time.Time `json:"upstream_response_at,omitempty"`
	UpstreamHeaderDurationMS int64      `json:"upstream_header_duration_ms,omitempty"`
	ResponseStatus           string     `json:"response_status,omitempty"`
	ResponseStatusCode       int        `json:"response_status_code,omitempty"`
	RequestContentEncoding   string     `json:"request_content_encoding,omitempty"`
	ResponseContentEncoding  string     `json:"response_content_encoding,omitempty"`
}

// Logger interface for dependency injection of logging functionality
type Logger interface {
	// LogRequest logs a request with its metadata and raw HTTP stream
	LogRequest(metadata RequestMetadata, timestamp time.Time, rawRequestStream io.ReadCloser)

	// LogResponse logs a response with its metadata and raw HTTP stream
	LogResponse(metadata RequestMetadata, timestamp time.Time, rawResponseStream io.ReadCloser)
}

// ConnectLogger is optionally implemented by loggers that want console-only CONNECT visibility.
type ConnectLogger interface {
	LogConnect(metadata RequestMetadata, timestamp time.Time)
}

// CaptureController is optionally implemented by a Logger to disable stream
// capture entirely. Loggers that do not implement it are capture-enabled by
// default, preserving compatibility with custom library integrations.
//
// A disabled logger is kept off the proxy hot path: request/response pipes,
// header reconstruction, and logging goroutines are not created.
type CaptureController interface {
	CaptureEnabled() bool
}

func loggerCaptureEnabled(logger Logger) bool {
	if logger == nil {
		return false
	}
	if controller, ok := logger.(CaptureController); ok {
		return controller.CaptureEnabled()
	}
	return true
}

// NoOpLogger is a logger that disables stream capture.
type NoOpLogger struct{}

// CaptureEnabled lets proxy handlers bypass all logging machinery when a
// NoOpLogger is configured.
func (n *NoOpLogger) CaptureEnabled() bool {
	return false
}

func (n *NoOpLogger) LogRequest(metadata RequestMetadata, timestamp time.Time, rawRequestStream io.ReadCloser) {
	// Must consume the stream to avoid blocking the TeeReader
	defer rawRequestStream.Close()
	io.Copy(io.Discard, rawRequestStream)
}

func (n *NoOpLogger) LogResponse(metadata RequestMetadata, timestamp time.Time, rawResponseStream io.ReadCloser) {
	// Must consume the stream to avoid blocking the TeeReader
	defer rawResponseStream.Close()
	io.Copy(io.Discard, rawResponseStream)
}
