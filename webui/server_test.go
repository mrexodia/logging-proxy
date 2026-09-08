package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	proxy "github.com/mrexodia/logging-proxy"
)

func put(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func eventLine(t *testing.T, e Event) []byte {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}
func fixture(t *testing.T, dir, id string, when time.Time, complete bool) Event {
	t.Helper()
	e := Event{Event: "request_started", StreamType: "request", Metadata: proxy.RequestMetadata{ID: id, Method: "GET", SourceURL: "https://example.test/" + id, RequestStartedAt: when}, Timestamp: when, StartedAt: when, Filename: id + "_request.bin"}
	data := eventLine(t, e)
	put(t, dir, e.Filename, []byte("GET / HTTP/1.1\r\n\r\n"))
	if complete {
		done := e
		done.Event = "request_completed"
		done.Completed = true
		data = append(data, eventLine(t, done)...)
		done.Event = "response_completed"
		done.StreamType = "response"
		done.Filename = id + "_response.bin"
		done.Metadata.ResponseStatusCode = 200
		data = append(data, eventLine(t, done)...)
		put(t, dir, done.Filename, []byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\ndata: hi\n\n"))
	}
	put(t, dir, id+"_metadata.jsonl", data)
	return e
}
func request(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
	return w
}

func TestHistoryReadOnlyPaginationLegacyAndFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	fixture(t, dir, "older", now.Add(-time.Hour), true)
	fixture(t, dir, "newer", now, false)
	legacy := fixture(t, dir, "migrated", now.Add(-2*time.Hour), true)
	put(t, dir, "migrated_request_metadata.json", bytes.TrimSpace(eventLine(t, legacy)))
	legacy.Metadata.ID = "legacy"
	legacy.Filename = "legacy_request.bin"
	legacy.Completed = true
	put(t, dir, legacy.Filename, []byte("legacy body"))
	put(t, dir, "legacy_request_metadata.json", bytes.TrimSpace(eventLine(t, legacy)))
	put(t, dir, "orphan_response.bin", []byte{0, 255, 1})
	put(t, dir, "archive.zip", []byte("archive"))
	put(t, dir, "broken_metadata.jsonl", []byte("invalid\n"))
	before := make(map[string][]byte)
	dates := make(map[string]time.Time)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		before[e.Name()], _ = os.ReadFile(filepath.Join(dir, e.Name()))
		info, _ := e.Info()
		dates[e.Name()] = info.ModTime()
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s)
	w := request(t, h, "/api/requests?limit=1")
	var result struct {
		Items []Transaction
		Total int
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 5 || len(result.Items) != 1 {
		t.Fatalf("unexpected page: %s", w.Body.String())
	}
	w = request(t, h, "/api/requests?q=older")
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result.Total != 1 || result.Items[0].State != "completed" || result.Items[0].Metadata.ResponseStatusCode != 200 {
		t.Fatalf("bad history: %s", w.Body.String())
	}
	w = request(t, h, "/api/requests?state=unfinished")
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result.Total != 3 {
		t.Fatalf("unfinished: %s", w.Body.String())
	}
	w = request(t, h, "/api/files?q=archive")
	if !strings.Contains(w.Body.String(), "archive.zip") {
		t.Fatal(w.Body.String())
	}
	w = request(t, h, "/api/file?name=orphan_response.bin&download=1")
	if !bytes.Equal(w.Body.Bytes(), []byte{0, 255, 1}) {
		t.Fatal("binary download changed")
	}
	for i := 0; i < 3; i++ {
		if err = s.Scan(); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Fatal("viewer changed file inventory")
	}
	for _, e := range after {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		info, _ := e.Info()
		if !bytes.Equal(data, before[e.Name()]) || !info.ModTime().Equal(dates[e.Name()]) {
			t.Fatalf("viewer modified %s", e.Name())
		}
	}
}

func TestPartialMetadataAndGrowth(t *testing.T) {
	dir := t.TempDir()
	e := fixture(t, dir, "stream", time.Now().UTC(), false)
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "stream_metadata.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	e.Event = "response_started"
	e.StreamType = "response"
	e.Filename = "stream_response.bin"
	e.Metadata.ResponseStatusCode = 200
	line := eventLine(t, e)
	_, _ = f.Write(line[:len(line)/2])
	if err = s.Scan(); err != nil {
		t.Fatal(err)
	}
	if len(s.transactions) != 1 || len(s.transactions[0].Events) != 1 {
		t.Fatal("partial line was ingested")
	}
	_, _ = f.Write(line[len(line)/2:])
	put(t, dir, e.Filename, []byte("first"))
	if err = s.Scan(); err != nil {
		t.Fatal(err)
	}
	if len(s.transactions[0].Events) != 2 || s.transactions[0].ResponseBytes != 5 {
		t.Fatal("completed line/body not visible")
	}
	revision := s.revision
	put(t, dir, e.Filename, []byte("first + more"))
	_ = s.Scan()
	if s.revision <= revision || s.transactions[0].ResponseBytes != 12 {
		t.Fatal("body growth not visible")
	}
}

func TestAccessBoundariesAndRawRanges(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte{0, 255, 10, 13, 42}, 20000)
	put(t, dir, "raw_response.bin", content)
	s, _ := NewStore(dir)
	h := NewHandler(s)
	for _, path := range []string{"/api/file?name=../secret.bin", "/api/file?name=C:%5Csecret.bin", "/api/file?name=raw_response.bin:secret", "/api/file?name=.env"} {
		if w := request(t, h, path); w.Code != 404 {
			t.Fatalf("path accessible: %s = %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/requests?limit=0", "/api/requests?offset=-1", "/api/file?name=raw_response.bin&offset=-1", "/api/tail?name=raw_response.bin&offset=bad"} {
		if w := request(t, h, path); w.Code != 400 {
			t.Fatalf("bad input accepted: %s = %d", path, w.Code)
		}
	}
	w := request(t, h, "/api/file?name=raw_response.bin&offset=65536")
	var chunk struct {
		Data       string
		Next, Size int64
	}
	_ = json.Unmarshal(w.Body.Bytes(), &chunk)
	data, _ := base64.StdEncoding.DecodeString(chunk.Data)
	if !bytes.Equal(data, content[65536:]) || chunk.Next != int64(len(content)) {
		t.Fatal("range read lost bytes")
	}
	for _, r := range []*http.Request{httptest.NewRequest("GET", "http://evil.test/api/files", nil), httptest.NewRequest("POST", "http://127.0.0.1/api/files", nil)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 && w.Code != 405 {
			t.Fatal("unsafe request accepted")
		}
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1/api/files", nil)
	r.Header.Set("Origin", "https://evil.test")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross origin request accepted")
	}
	for _, host := range []string{"0.0.0.0", "::", "example.com"} {
		if _, err := ListenAddress(host, 5602); err == nil {
			t.Fatal("nonlocal listener accepted")
		}
	}
}

type sseMessage struct {
	kind, id string
	data     []byte
}

func nextMessage(t *testing.T, reader *bufio.Reader) sseMessage {
	t.Helper()
	var m sseMessage
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" && m.kind != "" {
			return m
		}
		if strings.HasPrefix(line, "event: ") {
			m.kind = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "id: ") {
			m.id = strings.TrimPrefix(line, "id: ")
		}
		if strings.HasPrefix(line, "data: ") {
			m.data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
}

func TestTailReplaysResumesAndFollowsUntilCompletion(t *testing.T) {
	dir := t.TempDir()
	e := fixture(t, dir, "live", time.Now().UTC(), false)
	name := e.Filename
	put(t, dir, name, []byte{0, 255, 1, 2})
	s, _ := NewStore(dir)
	server := httptest.NewServer(NewHandler(s))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	open := func(offset string) *http.Response {
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/tail?name="+url.QueryEscape(name), nil)
		if offset != "" {
			req.Header.Set("Last-Event-ID", offset)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := open("")
	reader := bufio.NewReader(resp.Body)
	first := nextMessage(t, reader)
	if first.kind != "chunk" || first.id != "4" {
		t.Fatalf("bad replay: %+v", first)
	}
	_ = resp.Body.Close()
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte{3, 4, 255})
	_ = f.Close()
	resp = open(first.id)
	defer resp.Body.Close()
	reader = bufio.NewReader(resp.Body)
	second := nextMessage(t, reader)
	var chunk struct {
		Data   string
		Offset int64
	}
	_ = json.Unmarshal(second.data, &chunk)
	data, _ := base64.StdEncoding.DecodeString(chunk.Data)
	if second.id != "7" || chunk.Offset != 4 || !bytes.Equal(data, []byte{3, 4, 255}) {
		t.Fatal("resume lost or duplicated bytes")
	}
	// Append while the same HTTP stream is open; then publish completion.
	f, _ = os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND, 0600)
	_, _ = f.Write([]byte("last"))
	_ = f.Close()
	e.Event = "request_completed"
	e.Completed = true
	f, _ = os.OpenFile(filepath.Join(dir, "live_metadata.jsonl"), os.O_WRONLY|os.O_APPEND, 0600)
	_, _ = f.Write(eventLine(t, e))
	_ = f.Close()
	_ = s.Scan()
	third := nextMessage(t, reader)
	if third.kind != "chunk" || third.id != "11" {
		t.Fatalf("missed last append: %+v", third)
	}
	end := nextMessage(t, reader)
	if end.kind != "end" || end.id != "11" {
		t.Fatalf("bad end: %+v", end)
	}
}

func TestChangesAndEmbeddedAssets(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	server := httptest.NewServer(NewHandler(s))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first := nextMessage(t, reader)
	fixture(t, dir, "new", time.Now().UTC(), false)
	_ = s.Scan()
	second := nextMessage(t, reader)
	if first.kind != "change" || second.kind != "change" || bytes.Equal(first.data, second.data) {
		t.Fatal("change not delivered")
	}
	for _, path := range []string{"/", "/app.js", "/decoders.mjs", "/style.css"} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal(fmt.Sprint(path, resp.StatusCode))
		}
		if (strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".mjs")) && !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
			t.Fatalf("non-executable module MIME type: %s", resp.Header.Get("Content-Type"))
		}
		if resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatal("missing CSP")
		}
	}
}
