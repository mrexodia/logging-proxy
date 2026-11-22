package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	proxy "github.com/mrexodia/logging-proxy"
)

func dailyDir(t *testing.T, root, day string) string {
	t.Helper()
	dir := filepath.Join(root, day)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}

type requestPage struct {
	Items                          []Transaction
	Total, All, Unfinished, Errors int
	Days                           []string
}

func requestsPage(t *testing.T, h http.Handler, query string) requestPage {
	t.Helper()
	w := request(t, h, "/api/requests"+query)
	var result requestPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("request page: %d %s", w.Code, w.Body.String())
	}
	return result
}

func TestDailyHistoryFiltersAndPaths(t *testing.T) {
	root := t.TempDir()
	zone := time.FixedZone("original", 2*60*60)
	when := time.Date(2026, 7, 17, 0, 30, 0, 0, zone)
	fixture(t, root, "2026-07-16_flat", when.AddDate(0, 0, -1), true)
	firstDir := dailyDir(t, root, "2026-07-17")
	secondDir := dailyDir(t, root, "2026-07-18")
	// Same ID and basenames in different days must not collide or cross-link.
	fixture(t, firstDir, "same", when, true)
	fixture(t, secondDir, "same", when.AddDate(0, 0, 1), false)
	put(t, secondDir, "same_request.bin", []byte("second day's body"))
	put(t, firstDir, "archive.zip", []byte("archive"))
	put(t, firstDir, "orphan.bin", []byte("orphan"))
	put(t, firstDir, "broken_metadata.jsonl", []byte("invalid\n"))
	// A dated flat file should not be assigned today's modification date.
	put(t, root, "2026-07-16_orphan.bin", []byte("flat orphan"))

	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s)
	all := requestsPage(t, h, "")
	if all.Total != 4 || !reflect.DeepEqual(all.Days, []string{"2026-07-18", "2026-07-17", "2026-07-16"}) {
		t.Fatalf("unexpected mixed history: %+v", all)
	}
	filtered := requestsPage(t, h, "?day=2026-07-17&q=same&state=completed&method=GET")
	if filtered.Total != 1 || filtered.All != 2 || filtered.Unfinished != 1 {
		t.Fatalf("unexpected filtered counts: %+v", filtered)
	}
	item := filtered.Items[0]
	if item.Day != "2026-07-17" || item.RequestFile != "2026-07-17/same_request.bin" || item.ResponseFile != "2026-07-17/same_response.bin" || item.RequestBytes == 0 || item.ResponseBytes == 0 {
		t.Fatalf("incorrect daily paths: %+v", item)
	}
	other := requestsPage(t, h, "?day=2026-07-18").Items[0]
	if item.Key == other.Key || other.RequestBytes != int64(len("second day's body")) {
		t.Fatal("same basenames/IDs in different days collided")
	}
	if result := requestsPage(t, h, "?day=2026-07-17&limit=1&offset=1"); result.Total != 2 || len(result.Items) != 1 {
		t.Fatalf("day pagination failed: %+v", result)
	}
	if result := requestsPage(t, h, "?day=2026-07-16"); result.Total != 1 || result.Items[0].Metadata.ID != "2026-07-16_flat" {
		t.Fatalf("legacy start-date filter failed: %+v", result)
	}
	if result := requestsPage(t, h, "?day=2026-07-19"); result.Total != 0 || len(result.Days) != 3 {
		t.Fatalf("empty day/options: %+v", result)
	}
	w := request(t, h, "/api/requests/"+item.Key)
	var detail Transaction
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || len(detail.Events) != 3 {
		t.Fatalf("detail failed: %s", w.Body.String())
	}
	for _, event := range detail.Events {
		if !strings.HasPrefix(event.Filename, "2026-07-17/") {
			t.Fatalf("event filename not qualified: %q", event.Filename)
		}
	}

	w = request(t, h, "/api/files?day=2026-07-17&limit=2&offset=1")
	var files struct {
		Items []File
		Total int
		Days  []string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil || files.Total != 6 || len(files.Items) != 2 || len(files.Days) != 3 {
		t.Fatalf("file pagination: %s", w.Body.String())
	}
	for _, f := range files.Items {
		if f.Day != "2026-07-17" || !strings.HasPrefix(f.Name, "2026-07-17/") {
			t.Fatalf("file filter failed: %+v", f)
		}
	}
	w = request(t, h, "/api/files?day=2026-07-17&q=archive")
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil || files.Total != 1 || files.Items[0].Name != "2026-07-17/archive.zip" {
		t.Fatalf("file search: %s", w.Body.String())
	}
	w = request(t, h, "/api/file?name="+url.QueryEscape(other.RequestFile)+"&download=1")
	if w.Code != 200 || w.Body.String() != "second day's body" || w.Header().Get("Content-Disposition") != `attachment; filename="same_request.bin"` {
		t.Fatalf("daily download: %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "/api/file?name="+url.QueryEscape(other.RequestFile)+"&offset=7")
	var chunk struct{ Data string }
	_ = json.Unmarshal(w.Body.Bytes(), &chunk)
	data, _ := base64.StdEncoding.DecodeString(chunk.Data)
	if w.Code != 200 || string(data) != "day's body" {
		t.Fatalf("daily raw range: %s", w.Body.String())
	}
	// Repeated scans are read-only and unchanged files do not trigger updates.
	before, _ := os.ReadFile(filepath.Join(firstDir, "same_metadata.jsonl"))
	info, _ := os.Stat(filepath.Join(firstDir, "same_metadata.jsonl"))
	revision := s.revision
	if err := s.Scan(); err != nil || s.revision != revision {
		t.Fatalf("unchanged scan: %v, revision %d -> %d", err, revision, s.revision)
	}
	after, _ := os.ReadFile(filepath.Join(firstDir, "same_metadata.jsonl"))
	afterInfo, _ := os.Stat(filepath.Join(firstDir, "same_metadata.jsonl"))
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("scan modified daily metadata")
	}
	// Watching the root discovers directories created after viewer startup.
	thirdDir := dailyDir(t, root, "2026-07-19")
	fixture(t, thirdDir, "new", when.AddDate(0, 0, 2), false)
	if err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	if result := requestsPage(t, h, "?day=2026-07-19"); result.Total != 1 || result.Days[0] != "2026-07-19" || s.revision <= revision {
		t.Fatalf("new day not discovered: %+v", result)
	}
	if err := os.RemoveAll(thirdDir); err != nil {
		t.Fatal(err)
	}
	if err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	if result := requestsPage(t, h, "?day=2026-07-19"); result.Total != 0 || len(result.Days) != 3 {
		t.Fatalf("removed day retained: %+v", result)
	}
}

func TestDailyLoggerMidnightAndDirectDirectory(t *testing.T) {
	root := t.TempDir()
	logger, err := proxy.NewFileLogger(root, false)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 17, 23, 59, 59, 0, time.UTC)
	metadata := proxy.RequestMetadata{ID: "12345678", Method: "GET", RequestStartedAt: start}
	logger.LogRequest(metadata, start, io.NopCloser(strings.NewReader("request")))
	logger.LogResponse(metadata, start.Add(2*time.Second), io.NopCloser(strings.NewReader("response")))
	for _, dir := range []string{root, filepath.Join(root, "2026-07-17")} {
		s, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		page := requestsPage(t, NewHandler(s), "?day=2026-07-17")
		if page.Total != 1 || page.Items[0].State != "completed" || page.Items[0].RequestBytes != 7 || page.Items[0].ResponseBytes != 8 {
			t.Fatalf("logger integration for %s: %+v", dir, page)
		}
		if len(page.Days) != 1 || page.Days[0] != "2026-07-17" {
			t.Fatalf("midnight split the day: %v", page.Days)
		}
	}
}

func TestDailyLegacyMetadataAndUntrustedFilenames(t *testing.T) {
	root := t.TempDir()
	dir := dailyDir(t, root, "2026-07-17")
	// Legacy filename inference must stay relative to the metadata directory.
	// Older metadata omits the explicit completed flag.
	put(t, dir, "legacy_request_metadata.json", []byte(`{"metadata":{"id":"legacy"},"timestamp":"2026-07-17T12:00:00Z"}`))
	put(t, dir, "legacy_request.bin", []byte("legacy"))
	put(t, root, "outside.bin", []byte("not this transaction"))
	for i, name := range []string{"../outside.bin", "2026-07-18/other.bin", `..\outside.bin`, "/outside.bin"} {
		e := Event{Metadata: proxy.RequestMetadata{ID: name}, Event: "request_started", StreamType: "request", Filename: name}
		put(t, dir, string(rune('a'+i))+"_metadata.jsonl", eventLine(t, e))
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tx := range s.transactions {
		if tx.Metadata.ID == "legacy" {
			if tx.RequestFile != "2026-07-17/legacy_request.bin" || tx.RequestBytes != 6 || !s.finished(tx.RequestFile) {
				t.Fatalf("legacy resolution failed: %+v", tx)
			}
		} else if tx.RequestFile != "" {
			t.Fatalf("unsafe metadata body reference accepted: %+v", tx)
		}
	}
}

func TestDailyPathBoundaries(t *testing.T) {
	root := t.TempDir()
	dir := dailyDir(t, root, "2026-07-17")
	put(t, dir, "capture.bin", []byte("ok"))
	for _, name := range []string{"other", "2026-02-30", "2026-7-17", "2026-07-17/nested"} {
		fixture(t, dailyDir(t, root, name), "ignored", time.Now(), true)
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.files) != 1 || len(s.transactions) != 0 {
		t.Fatal("scanned arbitrary or nested directories")
	}
	h := NewHandler(s)
	for _, name := range []string{
		"../capture.bin", "2026-07-17/../capture.bin", "2026-07-17/../../secret.bin",
		"2026-07-17/nested/capture.bin", "other/capture.bin", "2026-02-30/capture.bin",
		"2026-07-17//capture.bin", "2026-07-17/./capture.bin", `2026-07-17\capture.bin`,
		"/2026-07-17/capture.bin", "2026-07-17/capture.bin:secret", "2026-07-17/.env",
	} {
		for _, endpoint := range []string{"/api/file", "/api/tail"} {
			w := request(t, h, endpoint+"?name="+url.QueryEscape(name))
			if w.Code != 404 && w.Code != 400 {
				t.Fatalf("invalid path accepted: %s %q = %d", endpoint, name, w.Code)
			}
		}
	}
	for _, day := range []string{"2026-02-30", "2026-7-17", "../", "2026-07-17/"} {
		for _, endpoint := range []string{"/api/requests", "/api/files"} {
			if w := request(t, h, endpoint+"?day="+url.QueryEscape(day)); w.Code != 400 {
				t.Fatalf("invalid day accepted: %q = %d", day, w.Code)
			}
		}
	}
	for _, kind := range []string{"directory", "file"} {
		t.Run("symlink-"+kind, func(t *testing.T) {
			outside := t.TempDir()
			put(t, outside, "secret.bin", []byte("synthetic secret"))
			target, link, name := outside, filepath.Join(root, "2026-07-18"), "2026-07-18/secret.bin"
			if kind == "file" {
				target, link, name = filepath.Join(outside, "secret.bin"), filepath.Join(dir, "link.bin"), "2026-07-17/link.bin"
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := s.Scan(); err != nil {
				t.Fatal(err)
			}
			if len(s.files) != 1 {
				t.Fatal("symlink indexed")
			}
			if w := request(t, h, "/api/file?name="+url.QueryEscape(name)); w.Code != 404 {
				t.Fatalf("symlink accessible: %d", w.Code)
			}
		})
	}
}

func TestDailyTailCompletion(t *testing.T) {
	root := t.TempDir()
	dir := dailyDir(t, root, "2026-07-17")
	e := fixture(t, dir, "live", time.Now(), false)
	put(t, dir, e.Filename, []byte("first"))
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(s))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/tail?name="+url.QueryEscape("2026-07-17/"+e.Filename), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if msg := nextMessage(t, reader); msg.kind != "chunk" || msg.id != "5" {
		t.Fatalf("daily replay: %+v", msg)
	}
	put(t, dir, e.Filename, []byte("first last"))
	e.Event, e.Completed = "request_completed", true
	f, err := os.OpenFile(filepath.Join(dir, "live_metadata.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write(eventLine(t, e))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("fixture append: %v %v", writeErr, closeErr)
	}
	if err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	if s.transactions[0].RequestBytes != 10 {
		t.Fatal("daily growth not reflected in history")
	}
	if msg := nextMessage(t, reader); msg.kind != "chunk" || msg.id != "10" {
		t.Fatalf("daily growth: %+v", msg)
	}
	if msg := nextMessage(t, reader); msg.kind != "end" || msg.id != "10" {
		t.Fatalf("daily completion: %+v", msg)
	}
}
