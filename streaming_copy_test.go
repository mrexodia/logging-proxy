package loggingproxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Deliberately lacks Flusher/FlushError, as some embedded ResponseWriters do.
type plainResponseWriter struct {
	body     bytes.Buffer
	header   http.Header
	writeErr error
}

func (w *plainResponseWriter) Header() http.Header { return w.header }
func (w *plainResponseWriter) WriteHeader(int)     {}
func (w *plainResponseWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(p)
}

type flushResponseWriter struct {
	plainResponseWriter
	flushed []string
	failAt  int
	err     error
}

func (w *flushResponseWriter) FlushError() error {
	w.flushed = append(w.flushed, w.body.String())
	if len(w.flushed) == w.failAt {
		return w.err
	}
	return nil
}

type wrappedResponseWriter struct{ http.ResponseWriter }

func (w wrappedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type chunkReader struct {
	chunks []string
	reads  int
	err    error
}

func (r *chunkReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.chunks) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func TestCopyProxyResponseFlushesWrappedWriter(t *testing.T) {
	writer := &flushResponseWriter{}
	source := &chunkReader{chunks: []string{"first", "second"}}
	n, err := copyProxyResponse(wrappedResponseWriter{writer}, source)
	if err != nil || n != 11 || writer.body.String() != "firstsecond" {
		t.Fatalf("copied %d bytes, body = %q, error = %v", n, writer.body.String(), err)
	}
	if got := strings.Join(writer.flushed, "|"); got != "|first|firstsecond" {
		t.Fatalf("flush snapshots = %q; want headers followed by each chunk", got)
	}
}

func TestCopyProxyResponseWithoutFlusher(t *testing.T) {
	writer := &plainResponseWriter{}
	n, err := copyProxyResponse(writer, strings.NewReader("body"))
	if err != nil || n != 4 || writer.body.String() != "body" {
		t.Fatalf("copied %d bytes, body = %q, error = %v", n, writer.body.String(), err)
	}
}

func TestCopyProxyResponseErrors(t *testing.T) {
	failure := errors.New("stream failed")
	for _, tc := range []struct {
		name      string
		flushAt   int
		writeErr  error
		readErr   error
		wantBytes int64
		wantReads int
	}{
		{name: "header-flush", flushAt: 1, wantReads: 0},
		{name: "body-flush", flushAt: 2, wantBytes: 5, wantReads: 1},
		{name: "write", writeErr: failure, wantReads: 1},
		{name: "read", readErr: failure, wantBytes: 11, wantReads: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &flushResponseWriter{failAt: tc.flushAt, err: failure}
			writer.writeErr = tc.writeErr
			source := &chunkReader{chunks: []string{"first", "second"}, err: tc.readErr}
			n, err := copyProxyResponse(writer, source)
			if !errors.Is(err, failure) || n != tc.wantBytes || source.reads != tc.wantReads {
				t.Fatalf("copied %d bytes in %d reads, error = %v; want %d bytes in %d reads and failure", n, source.reads, err, tc.wantBytes, tc.wantReads)
			}
		})
	}
}

func TestReverseProxyFlushFailureMarksCaptureIncomplete(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(strconv.FormatBool(upgrade), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "body")
			}))
			defer backend.Close()
			captured := make(chan streamCapture, 1)
			proxy := NewProxyServer("")
			if err := proxy.AddRoute("/", backend.URL+"/", &streamCaptureLogger{response: captured}); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("client disconnected")
			writer := &flushResponseWriter{failAt: 2, err: failure}
			writer.header = make(http.Header)
			request := httptest.NewRequest(http.MethodGet, "http://proxy/", nil)
			if upgrade {
				request.Header.Set("Connection", "Upgrade")
				request.Header.Set("Upgrade", "websocket")
			}
			proxy.ServeHTTP(writer, request)
			select {
			case got := <-captured:
				if !errors.Is(got.err, failure) {
					t.Fatalf("capture error = %v, want flush failure", got.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("capture remained open after flush failure")
			}
		})
	}
}

func TestReverseProxyStreamingClientDisconnect(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(strconv.FormatBool(capture), func(t *testing.T) {
			canceled := make(chan error, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				canceled <- nil
			}))
			defer backend.Close()
			captured := make(chan streamCapture, 1)
			var logger Logger = &NoOpLogger{}
			if capture {
				logger = &streamCaptureLogger{response: captured}
			}
			proxy := NewProxyServer("")
			if err := proxy.AddRoute("/", backend.URL+"/", logger); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(proxy)
			defer server.Close()
			client := server.Client()
			client.Timeout = 3 * time.Second
			response, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadFull(response.Body, make([]byte, len("data: first\n\n")))
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			awaitStreamResult(t, canceled, "upstream cancellation")
			if capture {
				select {
				case got := <-captured:
					if got.err == nil {
						t.Fatal("disconnected response capture incorrectly marked complete")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("capture remained open after client disconnected")
				}
			}
		})
	}
}
