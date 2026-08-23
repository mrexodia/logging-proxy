package loggingproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileLogger implements the Logger interface and writes logs to files.
type FileLogger struct {
	LogDir  string
	Console bool

	metadataMu sync.Mutex
}

// NewFileLogger creates a new file-based logger.
func NewFileLogger(logDir string, console bool) (*FileLogger, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	return &FileLogger{
		LogDir:  logDir,
		Console: console,
	}, nil
}

// LogRequest logs a request with its metadata and raw HTTP stream to a file.
func (f *FileLogger) LogRequest(metadata RequestMetadata, timestamp time.Time, rawRequestStream io.ReadCloser) {
	f.logRawStream(metadata, timestamp, rawRequestStream, "request")
}

// LogResponse logs a response with its metadata and raw HTTP stream to a file.
func (f *FileLogger) LogResponse(metadata RequestMetadata, timestamp time.Time, rawResponseStream io.ReadCloser) {
	f.logRawStream(metadata, timestamp, rawResponseStream, "response")
}

// LogConnect logs a CONNECT tunnel event to the console without creating disk logs.
func (f *FileLogger) LogConnect(metadata RequestMetadata, _ time.Time) {
	if !f.Console {
		return
	}
	log.Printf("[connect] %s: %s", shortMetadataID(metadata), formatConsoleRequest(metadata))
}

// fileLogEvent is one line in a transaction's metadata JSONL. Request and
// response events share a file and may interleave according to their actual
// completion order.
type fileLogEvent struct {
	Event        string          `json:"event"`
	StreamType   string          `json:"stream_type"`
	Metadata     RequestMetadata `json:"metadata"`
	Timestamp    time.Time       `json:"timestamp"`
	StartedAt    time.Time       `json:"started_at"`
	CompletedAt  *time.Time      `json:"completed_at,omitempty"`
	DurationMS   int64           `json:"duration_ms,omitempty"`
	BytesWritten int64           `json:"bytes_written"`
	Completed    bool            `json:"completed"`
	Error        string          `json:"error,omitempty"`
	Filename     string          `json:"filename"`
}

// logRawStream writes one request or response body and appends lifecycle events
// to the transaction-level metadata JSONL.
func (f *FileLogger) logRawStream(metadata RequestMetadata, timestamp time.Time, rawStream io.ReadCloser, streamType string) {
	defer rawStream.Close()

	timestampStr := timestamp.Format("2006-01-02_15-04-05.000")
	metadataID := shortMetadataID(metadata)
	filename := fmt.Sprintf("%s_%s_%s.bin", timestampStr, metadataID, streamType)
	filePath := filepath.Join(f.LogDir, filename)
	metadataPath := filepath.Join(f.LogDir, transactionMetadataFilename(metadata, timestamp))

	metadataFile, metadataErr := os.OpenFile(metadataPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if metadataErr != nil {
		log.Printf("[error] Failed to open metadata file %s: %v\n", metadataPath, metadataErr)
	}
	defer func() {
		if metadataFile != nil {
			if err := metadataFile.Close(); err != nil {
				log.Printf("[error] Failed to close metadata file %s: %v\n", metadataPath, err)
			}
		}
	}()

	startedEvent := fileLogEvent{
		Event:      streamType + "_started",
		StreamType: streamType,
		Metadata:   metadata,
		Timestamp:  timestamp,
		StartedAt:  timestamp,
		Filename:   filename,
	}
	f.appendMetadataEvent(metadataFile, metadataPath, startedEvent)

	logFile, createErr := os.Create(filePath)
	if createErr != nil {
		// Keep consuming the pipe so a logging-side filesystem failure does not
		// break or stall the proxied request.
		_, _ = io.Copy(io.Discard, rawStream)
		completedAt := time.Now()
		completedEvent := startedEvent
		completedEvent.Event = streamType + "_completed"
		completedEvent.Timestamp = completedAt
		completedEvent.CompletedAt = &completedAt
		completedEvent.DurationMS = completedAt.Sub(timestamp).Milliseconds()
		completedEvent.Error = fmt.Sprintf("failed to create log file: %v", createErr)
		f.appendMetadataEvent(metadataFile, metadataPath, completedEvent)
		log.Printf("[error] Failed to create log file %s: %v\n", filePath, createErr)
		return
	}

	bytesWritten, copyErr := io.Copy(logFile, rawStream)
	closeErr := logFile.Close()
	streamErr := errors.Join(copyErr, closeErr)
	completedAt := time.Now()
	completedEvent := startedEvent
	completedEvent.Event = streamType + "_completed"
	completedEvent.Timestamp = completedAt
	completedEvent.CompletedAt = &completedAt
	completedEvent.DurationMS = completedAt.Sub(timestamp).Milliseconds()
	completedEvent.BytesWritten = bytesWritten
	completedEvent.Completed = streamErr == nil
	if streamErr != nil {
		completedEvent.Error = streamErr.Error()
		log.Printf("[error] Failed to write raw HTTP stream: %v\n", streamErr)
	}
	f.appendMetadataEvent(metadataFile, metadataPath, completedEvent)

	if f.Console {
		log.Printf("[%s] %s: %s", streamType, metadataID, formatConsoleRequest(metadata))
		log.Printf("[%s] %s: %d bytes saved to %s", streamType, metadataID, bytesWritten, filename)
	}
}

func transactionMetadataFilename(metadata RequestMetadata, fallback time.Time) string {
	startedAt := metadata.RequestStartedAt
	if startedAt.IsZero() {
		startedAt = fallback
	}
	return fmt.Sprintf("%s_%s_metadata.jsonl", startedAt.Format("2006-01-02_15-04-05.000"), shortMetadataID(metadata))
}

func (f *FileLogger) appendMetadataEvent(metadataFile *os.File, metadataPath string, event fileLogEvent) {
	if metadataFile == nil {
		return
	}

	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("[error] Failed to encode metadata event for %s: %v\n", metadataPath, err)
		return
	}
	data = append(data, '\n')

	// Request and response loggers may hold separate append handles for the same
	// transaction. Serialize each complete JSON line so they cannot interleave.
	f.metadataMu.Lock()
	defer f.metadataMu.Unlock()
	for len(data) > 0 {
		written, writeErr := metadataFile.Write(data)
		if writeErr != nil {
			log.Printf("[error] Failed to append metadata event to %s: %v\n", metadataPath, writeErr)
			return
		}
		if written == 0 {
			log.Printf("[error] Failed to append metadata event to %s: short write\n", metadataPath)
			return
		}
		data = data[written:]
	}
}

func shortMetadataID(metadata RequestMetadata) string {
	if len(metadata.ID) <= 8 {
		return metadata.ID
	}
	return metadata.ID[:8]
}

func formatConsoleRequest(metadata RequestMetadata) string {
	if metadata.DestinationURL == "" || metadata.DestinationURL == metadata.SourceURL {
		return fmt.Sprintf("%s %s", metadata.Method, metadata.SourceURL)
	}
	return fmt.Sprintf("%s %s -> %s", metadata.Method, metadata.SourceURL, metadata.DestinationURL)
}
