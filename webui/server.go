package webui

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed assets/*
var assets embed.FS

// ListenAddress intentionally accepts loopback only: captures can contain full
// credentials and private bodies. Remote access can use an authenticated tunnel.
func ListenAddress(host string, port int) (string, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 5602
	}
	if !loopback(host) || port < 1 || port > 65535 {
		return "", fmt.Errorf("webui requires a loopback host and a port between 1 and 65535")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func NewHandler(store *Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/requests", store.list)
	mux.HandleFunc("GET /api/requests/{key}", func(w http.ResponseWriter, r *http.Request) {
		t, ok := store.transaction(r.PathValue("key"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, t)
	})
	mux.HandleFunc("GET /api/files", store.listFiles)
	mux.HandleFunc("GET /api/file", store.file)
	mux.HandleFunc("GET /api/tail", store.tail)
	mux.HandleFunc("GET /api/events", store.events)
	static, _ := fs.Sub(assets, "assets")
	fileServer := http.FileServer(http.FS(static))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Windows MIME registry settings can map .mjs to text/plain, which
		// browsers correctly refuse to execute as an ES module.
		if strings.HasSuffix(r.URL.Path, ".mjs") {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		fileServer.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		host := r.Host
		if parsed, _, err := net.SplitHostPort(host); err == nil {
			host = parsed
		}
		if !loopback(host) {
			http.Error(w, "Loopback host required", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				http.Error(w, "Cross-origin access denied", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Read-only viewer", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodHead && (r.URL.Path == "/api/tail" || r.URL.Path == "/api/events") {
			w.Header().Set("Content-Type", "text/event-stream")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func page(r *http.Request) (int, int, error) {
	limit, offset := 100, 0
	var err error
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil {
			return 0, 0, err
		}
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
		if err != nil {
			return 0, 0, err
		}
	}
	if limit < 1 || limit > 500 || offset < 0 {
		return 0, 0, fmt.Errorf("invalid pagination")
	}
	return limit, offset, nil
}

// Called with s.mu held. Options are independent of the current search/filter.
func (s *Store) daysLocked() []string {
	unique := make(map[string]bool)
	for _, f := range s.files {
		unique[f.Day] = true
	}
	for _, t := range s.transactions {
		unique[t.Day] = true
	}
	days := make([]string, 0, len(unique))
	for day := range unique {
		days = append(days, day)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days
}

func dayFilter(w http.ResponseWriter, r *http.Request) (string, bool) {
	day := r.URL.Query().Get("day")
	if day != "" && !validDay(day) {
		http.Error(w, "invalid day (expected YYYY-MM-DD)", http.StatusBadRequest)
		return "", false
	}
	return day, true
}

func (s *Store) list(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := page(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	day, ok := dayFilter(w, r)
	if !ok {
		return
	}
	query := strings.ToLower(r.URL.Query().Get("q"))
	state, method := r.URL.Query().Get("state"), r.URL.Query().Get("method")
	s.mu.RLock()
	items := make([]Transaction, 0, limit)
	total, all, unfinished, errors := 0, 0, 0, 0
	for _, t := range s.transactions {
		if day != "" && t.Day != day {
			continue
		}
		all++
		if t.State == "unfinished" {
			unfinished++
		}
		if t.State == "error" || t.Metadata.ResponseStatusCode >= 400 {
			errors++
		}
		if state != "" && t.State != state {
			continue
		}
		if method != "" && t.Metadata.Method != method {
			continue
		}
		search := t.Metadata.Method + " " + t.Metadata.SourceURL + " " + t.Metadata.DestinationURL + " " + t.Metadata.ID + " " + t.Metadata.Pattern + " " + strconv.Itoa(t.Metadata.ResponseStatusCode) + " " + strings.Join(t.MetadataFiles, " ")
		if query != "" && !strings.Contains(strings.ToLower(search), query) {
			continue
		}
		if total >= offset && len(items) < limit {
			t.Events = nil
			items = append(items, t)
		}
		total++
	}
	result := map[string]any{"items": items, "total": total, "all": all, "unfinished": unfinished, "errors": errors, "revision": s.revision, "problem": s.problem, "days": s.daysLocked()}
	s.mu.RUnlock()
	writeJSON(w, result)
}

func (s *Store) listFiles(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := page(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	day, ok := dayFilter(w, r)
	if !ok {
		return
	}
	query := strings.ToLower(r.URL.Query().Get("q"))
	s.mu.RLock()
	files := make([]File, 0, len(s.files))
	for _, f := range s.files {
		if (day == "" || f.Day == day) && strings.Contains(strings.ToLower(f.Name), query) {
			files = append(files, f)
		}
	}
	problem, days := s.problem, s.daysLocked()
	s.mu.RUnlock()
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	total := len(files)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, map[string]any{"items": files[offset:end], "total": total, "problem": problem, "days": days})
}

func offsetValue(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("offset")
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		value = id
	}
	if value == "" {
		return 0, nil
	}
	offset, err := strconv.ParseInt(value, 10, 64)
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("invalid byte offset")
	}
	return offset, nil
}

func (s *Store) file(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	f, err := s.open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "Cannot stat capture", 500)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(name)))
		http.ServeContent(w, r, name, info.ModTime(), f)
		return
	}
	offset, err := offsetValue(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if offset > info.Size() {
		http.Error(w, "Offset exceeds current capture size", 416)
		return
	}
	data := make([]byte, 64<<10)
	n, err := f.ReadAt(data, offset)
	if err != nil && err != io.EOF {
		http.Error(w, "Cannot read capture", 500)
		return
	}
	writeJSON(w, map[string]any{"data": base64.StdEncoding.EncodeToString(data[:n]), "offset": offset, "next": offset + int64(n), "size": info.Size()})
}

func eventStream(w http.ResponseWriter) (*http.ResponseController, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	return controller, controller.Flush()
}

func sendEvent(w http.ResponseWriter, controller *http.ResponseController, kind, id string, value any) error {
	_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err = fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data); err != nil {
		return err
	}
	err = controller.Flush()
	_ = controller.SetWriteDeadline(time.Time{})
	return err
}

func (s *Store) events(w http.ResponseWriter, r *http.Request) {
	controller, err := eventStream(w)
	if err != nil {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		s.mu.RLock()
		revision, wake, problem := s.revision, s.wake, s.problem
		s.mu.RUnlock()
		if err := sendEvent(w, controller, "change", "", map[string]any{"revision": revision, "problem": problem}); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-heartbeat.C:
		}
	}
}

func (s *Store) tail(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !strings.HasSuffix(name, ".bin") {
		http.Error(w, "Only capture streams can be followed", 400)
		return
	}
	offset, err := offsetValue(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f, err := s.open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "Cannot stat capture", 500)
		return
	}
	if offset > info.Size() {
		http.Error(w, "Offset exceeds current capture size", 416)
		return
	}
	controller, err := eventStream(w)
	if err != nil {
		return
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	data := make([]byte, 64<<10)
	lastHeartbeat := time.Now()
	for {
		if r.Context().Err() != nil {
			return
		}
		// Observe completion BEFORE reading EOF. This prevents a final append
		// between the read and completion lookup from being lost.
		finished := s.finished(name)
		n, readErr := f.ReadAt(data, offset)
		if n > 0 {
			next := offset + int64(n)
			if sendEvent(w, controller, "chunk", strconv.FormatInt(next, 10), map[string]any{"offset": offset, "next": next, "data": base64.StdEncoding.EncodeToString(data[:n])}) != nil {
				return
			}
			offset = next
			continue
		}
		if readErr != nil && readErr != io.EOF {
			_ = sendEvent(w, controller, "failure", "", map[string]string{"error": "Cannot read capture"})
			return
		}
		if finished {
			_ = sendEvent(w, controller, "end", strconv.FormatInt(offset, 10), map[string]any{"offset": offset})
			return
		}
		if time.Since(lastHeartbeat) >= 15*time.Second {
			if sendEvent(w, controller, "waiting", "", map[string]any{"offset": offset}) != nil {
				return
			}
			lastHeartbeat = time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
