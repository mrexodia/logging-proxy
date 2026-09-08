package loggingproxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// goproxy discards Hijack's buffered reader for plain HTTP upgrades. net/http
// can already have read the first frame bytes, even if the client waited for
// the 101 response. Return a connection which preserves those buffered bytes.
type bufferedUpgradeConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedUpgradeConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

type upgradeResponseWriter struct{ http.ResponseWriter }

func (w *upgradeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &bufferedUpgradeConn{Conn: conn, reader: buffer.Reader}, buffer, nil
}
func (w *upgradeResponseWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *upgradeResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func isWebSocketUpgrade(header http.Header) bool {
	has := func(name, token string) bool {
		for _, value := range header.Values(name) {
			for _, part := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(part), token) {
					return true
				}
			}
		}
		return false
	}
	return has("Connection", "upgrade") && has("Upgrade", "websocket")
}

// A 101 response body implements ReadWriteCloser. Keep that capability while
// capturing both directions; wrapping it as an ordinary HTTP body breaks the
// WebSocket tunnel. Disk files contain the handshake followed by exact frames.
type upgradeCapture struct {
	source                io.ReadWriteCloser
	request, response     *io.PipeWriter
	once                  sync.Once
	requestMu, responseMu sync.Mutex
	done                  chan struct{}
}

func newUpgradeCapture(ctx context.Context, source io.ReadWriteCloser, request, response *io.PipeWriter) *upgradeCapture {
	c := &upgradeCapture{source: source, request: request, response: response, done: make(chan struct{})}
	// goproxy's plain HTTP upgrade path closes the original upstream body, not
	// the replacement. End our capture when its handler context is canceled too.
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = c.Close()
			case <-c.done:
			}
		}()
	}
	return c
}

func (c *upgradeCapture) Read(p []byte) (int, error) {
	c.responseMu.Lock()
	defer c.responseMu.Unlock()
	n, err := c.source.Read(p)
	if n > 0 {
		_, _ = c.response.Write(p[:n])
	}
	if err != nil {
		_ = c.response.Close()
	}
	return n, err
}
func (c *upgradeCapture) Write(p []byte) (int, error) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	n, err := c.source.Write(p)
	if n > 0 {
		_, _ = c.request.Write(p[:n])
	}
	if err != nil {
		_ = c.request.Close()
	}
	return n, err
}
func (c *upgradeCapture) Close() error {
	var err error
	c.once.Do(func() {
		err = c.source.Close()
		c.requestMu.Lock()
		_ = c.request.Close()
		c.requestMu.Unlock()
		c.responseMu.Lock()
		_ = c.response.Close()
		c.responseMu.Unlock()
		close(c.done)
	})
	return err
}

func startUpgradeLog(logger Logger, metadata RequestMetadata, timestamp time.Time, firstLine string, headers http.Header, response bool) *io.PipeWriter {
	reader, writer := io.Pipe()
	var prefix bytes.Buffer
	fmt.Fprintf(&prefix, "%s\r\n", firstLine)
	for name, values := range headers {
		if strings.EqualFold(name, "Proxy-Authorization") || strings.EqualFold(name, "Proxy-Authenticate") {
			continue
		}
		for _, value := range values {
			fmt.Fprintf(&prefix, "%s: %s\r\n", name, value)
		}
	}
	prefix.WriteString("\r\n")
	go func() {
		defer reader.Close()
		stream := &readCloser{Reader: io.MultiReader(&prefix, reader), Closer: io.NopCloser(nil)}
		if response {
			logger.LogResponse(metadata, timestamp, stream)
		} else {
			logger.LogRequest(metadata, timestamp, stream)
		}
	}()
	return writer
}

func (s *ProxyServer) handleWebSocket(w http.ResponseWriter, r *http.Request, destination url.URL, logger Logger) {
	started := time.Now()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	metadata := RequestMetadata{ID: uuid.NewString(), Pattern: r.Pattern, Method: r.Method, SourceURL: scheme + "://" + r.Host + r.URL.String(), DestinationURL: destination.String(), RequestStartedAt: started}
	r.URL = &destination
	r.Host = destination.Host
	r.RequestURI = ""
	var requestWriter *io.PipeWriter
	if loggerCaptureEnabled(logger) {
		requestWriter = startUpgradeLog(logger, metadata, started, fmt.Sprintf("%s %s %s", r.Method, destination.String(), r.Proto), r.Header, false)
		defer requestWriter.Close()
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &readCloser{Reader: io.TeeReader(r.Body, requestWriter), Closer: r.Body}
		}
	}
	response, err := s.client.Do(r)
	if err != nil {
		http.Error(w, "WebSocket upstream failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	responseTime := time.Now()
	metadata.UpstreamResponseAt = &responseTime
	metadata.UpstreamHeaderDurationMS = responseTime.Sub(started).Milliseconds()
	metadata.ResponseStatus = response.Status
	metadata.ResponseStatusCode = response.StatusCode
	var responseWriter *io.PipeWriter
	if requestWriter != nil {
		responseWriter = startUpgradeLog(logger, metadata, responseTime, response.Proto+" "+response.Status, response.Header, true)
		defer responseWriter.Close()
	}
	if response.StatusCode != http.StatusSwitchingProtocols || !isWebSocketUpgrade(response.Header) {
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		var body io.Reader = response.Body
		if responseWriter != nil {
			body = io.TeeReader(body, responseWriter)
		}
		_, _ = copyProxyStream(w, body)
		return
	}
	upstream, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		http.Error(w, "Upstream upgrade is not bidirectional", http.StatusBadGateway)
		return
	}
	client, buffer, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "WebSocket upgrade requires HTTP/1.1", http.StatusBadGateway)
		return
	}
	defer client.Close()
	// Preserve bytes already buffered by the HTTP server after the handshake.
	if _, err = fmt.Fprintf(buffer, "HTTP/1.1 %s\r\n", response.Status); err != nil {
		return
	}
	if err = response.Header.Write(buffer); err != nil {
		return
	}
	if _, err = buffer.WriteString("\r\n"); err != nil {
		return
	}
	if err = buffer.Flush(); err != nil {
		return
	}
	if requestWriter != nil {
		upstream = newUpgradeCapture(r.Context(), upstream, requestWriter, responseWriter)
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buffer); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}
