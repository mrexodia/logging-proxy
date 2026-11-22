package loggingproxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatConsoleRequestOmitsRedundantDestination(t *testing.T) {
	target := "https://api.anthropic.com:443/v1/mcp_servers?limit=1000"
	got := formatConsoleRequest(RequestMetadata{
		Method:         http.MethodGet,
		SourceURL:      target,
		DestinationURL: target,
	})

	want := "GET " + target
	if got != want {
		t.Fatalf("formatConsoleRequest() = %q, want %q", got, want)
	}
}

func TestFormatConsoleRequestKeepsDifferentDestination(t *testing.T) {
	got := formatConsoleRequest(RequestMetadata{
		Method:         http.MethodPost,
		SourceURL:      "http://localhost:5601/anthropic/v1/messages",
		DestinationURL: "https://api.anthropic.com/v1/messages",
	})

	want := "POST http://localhost:5601/anthropic/v1/messages -> https://api.anthropic.com/v1/messages"
	if got != want {
		t.Fatalf("formatConsoleRequest() = %q, want %q", got, want)
	}
}

func TestFileLoggingUsesOneTransactionMetadataJSONL(t *testing.T) {
	logDir := t.TempDir()
	fileLogger, err := NewFileLogger(logDir, false)
	if err != nil {
		t.Fatalf("failed to create file logger: %v", err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"message":"ok","path":%q}`, request.URL.Path)
	}))
	defer backend.Close()

	server := NewProxyServer("")
	if err := server.AddRoute("/api/", backend.URL+"/", fileLogger); err != nil {
		t.Fatalf("failed to add route: %v", err)
	}
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	response, err := http.Post(proxy.URL+"/api/test", "application/json", strings.NewReader(`{"test":"data"}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("failed to consume response: read=%v close=%v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	metadataPath, events := waitForFileLogEvents(t, logDir, 4)
	if filepath.Ext(metadataPath) != ".jsonl" {
		t.Fatalf("metadata path = %q, want .jsonl", metadataPath)
	}

	wantEvents := map[string]bool{
		"request_started":    false,
		"request_completed":  false,
		"response_started":   false,
		"response_completed": false,
	}
	var transactionID string
	for _, event := range events {
		if _, ok := wantEvents[event.Event]; !ok {
			t.Fatalf("unexpected metadata event %q", event.Event)
		}
		if wantEvents[event.Event] {
			t.Fatalf("duplicate metadata event %q", event.Event)
		}
		wantEvents[event.Event] = true
		if transactionID == "" {
			transactionID = event.Metadata.ID
		} else if event.Metadata.ID != transactionID {
			t.Fatalf("event %q ID = %q, want %q", event.Event, event.Metadata.ID, transactionID)
		}
		if event.Filename == "" {
			t.Fatalf("event %q has no body filename", event.Event)
		}
		if strings.HasSuffix(event.Event, "_started") && event.Completed {
			t.Fatalf("started event %q is marked completed", event.Event)
		}
		if strings.HasSuffix(event.Event, "_completed") {
			if !event.Completed {
				t.Fatalf("completed event %q has error %q", event.Event, event.Error)
			}
			if event.CompletedAt == nil || event.BytesWritten == 0 {
				t.Fatalf("completed event %q is missing completion data", event.Event)
			}
		}
		if strings.HasPrefix(event.Event, "response_") && event.Metadata.ResponseStatusCode != http.StatusOK {
			t.Fatalf("response event status = %d, want 200", event.Metadata.ResponseStatusCode)
		}
	}
	for event, found := range wantEvents {
		if !found {
			t.Fatalf("missing metadata event %q", event)
		}
	}

	dayDir := filepath.Dir(metadataPath)
	if filepath.Base(dayDir) != events[0].Metadata.RequestStartedAt.Format("2006-01-02") {
		t.Fatalf("metadata directory = %q, want request start date", dayDir)
	}
	entries, err := os.ReadDir(dayDir)
	if err != nil {
		t.Fatalf("failed to read log directory: %v", err)
	}
	requestFiles, responseFiles, metadataFiles := 0, 0, 0
	for _, entry := range entries {
		switch {
		case strings.HasSuffix(entry.Name(), "_request.bin"):
			requestFiles++
		case strings.HasSuffix(entry.Name(), "_response.bin"):
			responseFiles++
		case strings.HasSuffix(entry.Name(), "_metadata.jsonl"):
			metadataFiles++
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			t.Fatalf("failed to inspect %s: %v", entry.Name(), infoErr)
		}
		if info.Size() == 0 {
			t.Fatalf("file %s is empty", entry.Name())
		}
	}
	if requestFiles != 1 || responseFiles != 1 || metadataFiles != 1 {
		t.Fatalf("files: request=%d response=%d metadata=%d, want 1 each", requestFiles, responseFiles, metadataFiles)
	}

	// Completed logger calls must not leave file handles open.
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dayDir, entry.Name())); err != nil {
			t.Fatalf("failed to remove %s: %v", entry.Name(), err)
		}
	}
}

func TestFileLoggingPublishesStartedEventWhileStreamIsOpen(t *testing.T) {
	logDir := t.TempDir()
	fileLogger, err := NewFileLogger(logDir, false)
	if err != nil {
		t.Fatalf("failed to create file logger: %v", err)
	}

	startedAt := time.Now()
	metadata := RequestMetadata{
		ID:               "12345678-1234-1234-1234-123456789abc",
		Pattern:          "/api/",
		Method:           http.MethodPost,
		SourceURL:        "http://proxy.test/api/",
		DestinationURL:   "https://backend.test/",
		RequestStartedAt: startedAt,
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		fileLogger.LogRequest(metadata, startedAt, reader)
		close(done)
	}()

	_, events := waitForFileLogEvents(t, logDir, 1)
	if len(events) != 1 || events[0].Event != "request_started" || events[0].Completed {
		t.Fatalf("ongoing events = %#v, want one incomplete request_started", events)
	}

	if _, err := writer.Write([]byte("stream body")); err != nil {
		t.Fatalf("failed to write stream: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close stream: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logger did not finish")
	}

	_, events = waitForFileLogEvents(t, logDir, 2)
	if events[1].Event != "request_completed" || !events[1].Completed {
		t.Fatalf("final event = %#v, want completed request", events[1])
	}
}

func TestFileLoggingDailyDirectories(t *testing.T) {
	logDir := t.TempDir()
	logger, err := NewFileLogger(logDir, false)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 17, 23, 59, 59, 0, time.FixedZone("test", 2*60*60))
	metadata := RequestMetadata{ID: "12345678", RequestStartedAt: start}
	logger.LogRequest(metadata, start, io.NopCloser(strings.NewReader("request")))
	logger.LogResponse(metadata, start.Add(2*time.Second), io.NopCloser(strings.NewReader("response")))
	path, events := waitForFileLogEvents(t, logDir, 4)
	if filepath.Dir(path) != filepath.Join(logDir, "2026-07-17") {
		t.Fatalf("metadata path = %q, want request date directory", path)
	}
	for _, event := range events {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), event.Filename)); err != nil {
			t.Fatalf("body missing beside metadata: %v", err)
		}
	}

	// The same logger rolls over for new transactions, including callers that
	// omit RequestStartedAt and rely on the supplied timestamp.
	logger.LogRequest(RequestMetadata{ID: "87654321"}, start.Add(2*time.Second), io.NopCloser(strings.NewReader("next day")))
	entries, err := os.ReadDir(filepath.Join(logDir, "2026-07-18"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("next day's files = %v, error = %v; want body and metadata", entries, err)
	}
	entries, err = os.ReadDir(logDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("root entries = %v, error = %v; want two day directories", entries, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("unexpected root file %q", entry.Name())
		}
	}
}

func TestFileLoggingDrainsStreamWhenDailyDirectoryCreationFails(t *testing.T) {
	logDir := t.TempDir()
	logger, err := NewFileLogger(logDir, false)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(logDir, "2026-07-18"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	stream := strings.NewReader("must be consumed")
	logger.LogRequest(RequestMetadata{ID: "12345678"}, start, io.NopCloser(stream))
	if stream.Len() != 0 {
		t.Fatal("logger did not drain stream after directory creation failed")
	}
}

func waitForFileLogEvents(t *testing.T, logDir string, minimum int) (string, []fileLogEvent) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(filepath.Join(logDir, "*", "*_metadata.jsonl"))
		if err != nil {
			t.Fatalf("failed to find metadata: %v", err)
		}
		if len(matches) > 1 {
			t.Fatalf("found %d transaction metadata files, want one", len(matches))
		}
		if len(matches) == 1 {
			events, readErr := readFileLogEvents(matches[0])
			if readErr == nil && len(events) >= minimum {
				return matches[0], events
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d metadata events", minimum)
	return "", nil
}

func readFileLogEvents(path string) ([]fileLogEvent, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var events []fileLogEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event fileLogEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
