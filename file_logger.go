package loggingproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
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

// LogRequest logs a request with its metadata and raw HTTP stream to a file
func (f *FileLogger) LogRequest(metadata RequestMetadata, timestamp time.Time, rawRequestStream io.ReadCloser) {
	f.logRawStream(metadata, timestamp, rawRequestStream, "request")
}

// LogResponse logs a response with its metadata and raw HTTP stream to a file
func (f *FileLogger) LogResponse(metadata RequestMetadata, timestamp time.Time, rawResponseStream io.ReadCloser) {
	f.logRawStream(metadata, timestamp, rawResponseStream, "response")
}

type fileLogMetadata struct {
	StreamType string          `json:"stream_type"`
	Metadata   RequestMetadata `json:"metadata"`
	Timestamp  time.Time       `json:"timestamp"`
	Filename   string          `json:"filename"`
}

// logRawStream handles the common logic for logging raw HTTP streams
func (f *FileLogger) logRawStream(metadata RequestMetadata, timestamp time.Time, rawStream io.ReadCloser, streamType string) {
	defer rawStream.Close()

	timestampStr := timestamp.Format("2006-01-02_15-04-05.000")
	filename := fmt.Sprintf("%s_%s_%s.bin", timestampStr, metadata.ID[:8], streamType)
	filePath := filepath.Join(f.LogDir, filename)

	// Create the log file
	logFile, err := os.Create(filePath)
	if err != nil {
		fmt.Printf("Failed to create log file %s: %v\n", filePath, err)
		return
	}
	defer logFile.Close()

	// Write raw HTTP stream (headers + body already combined)
	bytesWritten, err := io.Copy(logFile, rawStream)
	if err != nil {
		fmt.Printf("Failed to write raw HTTP stream: %v\n", err)
		return
	}

	// Create and save metadata
	logMetadata := fileLogMetadata{
		StreamType: streamType,
		Metadata:   metadata,
		Timestamp:  timestamp,
		Filename:   filename,
	}

	metadataFilename := fmt.Sprintf("%s_%s_%s_metadata.json", timestampStr, metadata.ID[:8], streamType)
	metadataPath := filepath.Join(f.LogDir, metadataFilename)

	metadataFile, err := os.Create(metadataPath)
	if err != nil {
		fmt.Printf("Failed to create metadata file %s: %v\n", metadataPath, err)
		return
	}
	defer metadataFile.Close()

	encoder := json.NewEncoder(metadataFile)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(logMetadata)
	if err != nil {
		fmt.Printf("Failed to write metadata file %s: %v\n", metadataPath, err)
		return
	}

	log.Printf("Saved %s %s (%d bytes) -> %s", streamType, metadata.ID[:8], bytesWritten, filename)
}
