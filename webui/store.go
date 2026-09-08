// Package webui provides a read-only, disk-backed traffic inspector. It never
// participates in the proxy's request path or writes to the capture directory.
package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	proxy "github.com/mrexodia/logging-proxy"
)

type Event struct {
	Event        string                `json:"event"`
	StreamType   string                `json:"stream_type"`
	Metadata     proxy.RequestMetadata `json:"metadata"`
	Timestamp    time.Time             `json:"timestamp"`
	StartedAt    time.Time             `json:"started_at"`
	CompletedAt  *time.Time            `json:"completed_at,omitempty"`
	Completed    bool                  `json:"completed"`
	BytesWritten int64                 `json:"bytes_written"`
	DurationMS   int64                 `json:"duration_ms,omitempty"`
	Error        string                `json:"error,omitempty"`
	Filename     string                `json:"filename"`
}

type File struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Problem  string    `json:"problem,omitempty"`
}

type Transaction struct {
	Key           string                `json:"key"`
	Metadata      proxy.RequestMetadata `json:"metadata"`
	Started       time.Time             `json:"started"`
	State         string                `json:"state"`
	RequestFile   string                `json:"request_file,omitempty"`
	ResponseFile  string                `json:"response_file,omitempty"`
	RequestBytes  int64                 `json:"request_bytes"`
	ResponseBytes int64                 `json:"response_bytes"`
	DurationMS    int64                 `json:"duration_ms"`
	Events        []Event               `json:"events,omitempty"`
	MetadataFiles []string              `json:"metadata_files,omitempty"`
	Problems      []string              `json:"problems,omitempty"`
}

type cachedMetadata struct {
	file    File
	events  []Event
	problem string
}

func (c cachedMetadata) complete() bool {
	request, response := false, false
	for _, e := range c.events {
		request = request || e.Event == "request_completed"
		response = response || e.Event == "response_completed"
	}
	return request && response
}

type Store struct {
	dir          string
	scanMu       sync.Mutex
	mu           sync.RWMutex
	cache        map[string]cachedMetadata
	files        map[string]File
	transactions []Transaction
	revision     uint64
	problem      string
	wake         chan struct{}
}

func NewStore(directory string) (*Store, error) {
	dir, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("log path is not a directory")
	}
	s := &Store{dir: dir, cache: make(map[string]cachedMetadata), files: make(map[string]File), wake: make(chan struct{})}
	if err := s.Scan(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Scan()
		}
	}
}

func allowedFile(name string) bool {
	return name != "" && filepath.Base(name) == name && !strings.ContainsAny(name, `/\:`) &&
		(strings.HasSuffix(name, ".bin") || strings.HasSuffix(name, "_metadata.jsonl") || strings.HasSuffix(name, "_metadata.json") || strings.HasSuffix(name, ".zip"))
}

// Open only regular files immediately inside the selected log directory. In
// particular, never follow a symlink planted in a log directory.
func (s *Store) open(name string) (*os.File, error) {
	if !allowedFile(name) {
		return nil, os.ErrNotExist
	}
	path := filepath.Join(s.dir, name)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, os.ErrPermission
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, os.ErrPermission
	}
	return f, nil
}

// Scan caches unchanged metadata; growing bodies are only stat'ed, never read.
// Partial final JSONL lines are retried after the writer appends more bytes.
func (s *Store) Scan() error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		s.mu.Lock()
		s.problem = "Cannot read log directory: " + err.Error()
		s.signalLocked()
		s.mu.Unlock()
		return err
	}
	files := make(map[string]File)
	cache := make(map[string]cachedMetadata)
	for _, entry := range entries {
		if !allowedFile(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		file := File{Name: entry.Name(), Size: info.Size(), Modified: info.ModTime()}
		if strings.HasSuffix(file.Name, "_metadata.jsonl") || strings.HasSuffix(file.Name, "_metadata.json") {
			cached, ok := s.cache[file.Name]
			if !ok || !cached.complete() || cached.file.Size != file.Size || !cached.file.Modified.Equal(file.Modified) {
				cached = s.readMetadata(file)
			}
			file = cached.file
			file.Problem = cached.problem
			cache[file.Name] = cached
		}
		files[file.Name] = file
	}
	// Windows directory entries can retain old sizes while a writer keeps its
	// handle open. Stat active captures through a read handle so long streams
	// remain visible before the logging goroutine closes them.
	for _, cached := range cache {
		if cached.complete() {
			continue
		}
		for _, event := range cached.events {
			if file, ok := files[event.Filename]; ok {
				if f, err := s.open(event.Filename); err == nil {
					if info, err := f.Stat(); err == nil {
						file.Size = info.Size()
						file.Modified = info.ModTime()
						files[file.Name] = file
					}
					_ = f.Close()
				}
			}
		}
	}
	s.mu.RLock()
	changed := len(files) != len(s.files) || s.problem != ""
	if !changed {
		for name, file := range files {
			if file != s.files[name] {
				changed = true
				break
			}
		}
	}
	s.mu.RUnlock()
	if !changed {
		return nil
	}

	groups := make(map[string]*Transaction)
	// Prefer JSONL when both migrated and legacy metadata describe the same ID.
	modernIDs := make(map[string]bool)
	for name, cached := range cache {
		if strings.HasSuffix(name, ".jsonl") {
			for _, event := range cached.events {
				if event.Metadata.ID != "" {
					modernIDs[event.Metadata.ID] = true
				}
			}
		}
	}
	names := make([]string, 0, len(cache))
	for name := range cache {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cached := cache[name]
		groupID := name
		if len(cached.events) > 0 && cached.events[0].Metadata.ID != "" {
			groupID = cached.events[0].Metadata.ID
		}
		if strings.HasSuffix(name, ".json") && modernIDs[groupID] {
			continue
		}
		transaction := groups[groupID]
		if transaction == nil {
			hash := sha256.Sum256([]byte(groupID))
			transaction = &Transaction{Key: hex.EncodeToString(hash[:16]), Started: cached.file.Modified, State: "unfinished"}
			groups[groupID] = transaction
		}
		transaction.MetadataFiles = append(transaction.MetadataFiles, name)
		transaction.Events = append(transaction.Events, cached.events...)
		if cached.problem != "" {
			transaction.Problems = append(transaction.Problems, name+": "+cached.problem)
		}
	}
	transactions := make([]Transaction, 0, len(groups))
	for _, t := range groups {
		sort.SliceStable(t.Events, func(i, j int) bool { return t.Events[i].Timestamp.Before(t.Events[j].Timestamp) })
		requestDone, responseDone := false, false
		for i, event := range t.Events {
			if i == 0 {
				t.Metadata = event.Metadata
				t.Started = event.StartedAt
				if t.Started.IsZero() {
					t.Started = event.Timestamp
				}
			}
			if !event.Metadata.RequestStartedAt.IsZero() {
				t.Started = event.Metadata.RequestStartedAt
			}
			if event.Metadata.ResponseStatusCode != 0 {
				t.Metadata = event.Metadata
			}
			switch event.StreamType {
			case "request":
				t.RequestFile = event.Filename
				requestDone = requestDone || event.Event == "request_completed"
			case "response":
				t.ResponseFile = event.Filename
				responseDone = responseDone || event.Event == "response_completed"
			}
			if event.Error != "" {
				t.State = "error"
				t.Problems = append(t.Problems, event.Error)
			}
			if event.CompletedAt != nil {
				duration := event.CompletedAt.Sub(t.Started).Milliseconds()
				if duration > t.DurationMS {
					t.DurationMS = duration
				}
			}
		}
		if t.State != "error" && requestDone && responseDone {
			t.State = "completed"
		}
		t.RequestBytes = files[t.RequestFile].Size
		t.ResponseBytes = files[t.ResponseFile].Size
		transactions = append(transactions, *t)
	}
	sort.Slice(transactions, func(i, j int) bool {
		if transactions[i].Started.Equal(transactions[j].Started) {
			return transactions[i].Key > transactions[j].Key
		}
		return transactions[i].Started.After(transactions[j].Started)
	})
	s.cache = cache
	s.mu.Lock()
	s.files, s.transactions, s.problem = files, transactions, ""
	s.signalLocked()
	s.mu.Unlock()
	return nil
}

func (s *Store) signalLocked() { s.revision++; close(s.wake); s.wake = make(chan struct{}) }

func (s *Store) readMetadata(file File) cachedMetadata {
	result := cachedMetadata{file: file}
	f, err := s.open(file.Name)
	if err != nil {
		result.problem = err.Error()
		return result
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil {
		result.file.Size = info.Size()
		result.file.Modified = info.ModTime()
	}
	const maxMetadata = 4 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxMetadata+1))
	if err != nil {
		result.problem = err.Error()
		return result
	}
	if len(data) > maxMetadata {
		result.problem = "Metadata exceeds 4 MiB; download the original to inspect it"
		return result
	}
	if strings.HasSuffix(file.Name, ".jsonl") {
		lines := bytes.Split(data, []byte{'\n'})
		for _, line := range lines[:len(lines)-1] {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var event Event
			if err := json.Unmarshal(line, &event); err != nil {
				result.problem = "Invalid metadata line; original file retained"
				continue
			}
			result.events = append(result.events, event)
		}
		if len(lines[len(lines)-1]) > 0 {
			result.problem = "Waiting for a complete metadata line (or interrupted write)"
		}
		return result
	}
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		result.problem = "Incomplete or invalid legacy metadata"
		return result
	}
	if event.StreamType == "" {
		if strings.HasSuffix(file.Name, "_request_metadata.json") {
			event.StreamType = "request"
		} else {
			event.StreamType = "response"
		}
	}
	if event.Filename == "" {
		event.Filename = strings.TrimSuffix(file.Name, "_metadata.json") + ".bin"
	}
	if event.StartedAt.IsZero() {
		event.StartedAt = event.Timestamp
	}
	event.Event = event.StreamType + "_started"
	result.events = append(result.events, event)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	_, explicit := fields["completed"]
	if !explicit {
		if f, err := s.open(event.Filename); err == nil {
			event.Completed = true
			f.Close()
		}
	}
	if event.Completed || event.CompletedAt != nil || event.Error != "" {
		event.Event = event.StreamType + "_completed"
		if event.CompletedAt != nil {
			event.Timestamp = *event.CompletedAt
		}
		result.events = append(result.events, event)
	}
	return result
}

func (s *Store) transaction(key string) (Transaction, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.transactions {
		if t.Key == key {
			return t, true
		}
	}
	return Transaction{}, false
}

func (s *Store) finished(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.transactions {
		for _, e := range t.Events {
			if e.Filename == name && strings.HasSuffix(e.Event, "_completed") {
				return true
			}
		}
	}
	return false
}
