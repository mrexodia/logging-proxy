package loggingproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type streamCapture struct {
	body []byte
	err  error
}

type streamCaptureLogger struct {
	response chan streamCapture
}

func (l *streamCaptureLogger) LogRequest(_ RequestMetadata, _ time.Time, body io.ReadCloser) {
	defer body.Close()
	_, _ = io.Copy(io.Discard, body)
}

func (l *streamCaptureLogger) LogResponse(_ RequestMetadata, _ time.Time, body io.ReadCloser) {
	defer body.Close()
	data, err := io.ReadAll(body)
	l.response <- streamCapture{body: data, err: err}
}

func awaitStreamResult(t *testing.T, result <-chan error, stage string) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: proxy buffered data while upstream was waiting", stage)
	}
}

// Each upstream write is gated by the client's receipt of the previous one.
// Checking only the completed body cannot distinguish streaming from buffering.
func TestReverseProxyFlushesResponse(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, http2 := range []bool{false, true} {
			for _, tc := range []struct {
				name        string
				contentType string
				fixedLength bool
				upgrade     bool
			}{
				{name: "sse", contentType: "text/event-stream"},
				{name: "ndjson", contentType: "application/x-ndjson"},
				{name: "fixed-length", contentType: "application/octet-stream", fixedLength: true},
				{name: "rejected-upgrade", contentType: "text/plain", upgrade: true},
			} {
				if http2 && tc.upgrade {
					continue // HTTP/1.1 Upgrade headers are not valid in HTTP/2.
				}
				t.Run(fmt.Sprintf("capture=%v/http2=%v/%s", capture, http2, tc.name), func(t *testing.T) {
					const first, second = "data: first\n\n", "data: second\n\n"
					status := http.StatusOK
					if tc.upgrade {
						status = http.StatusBadRequest
					}
					allowFirst, allowSecond, allowFinish := make(chan struct{}), make(chan struct{}), make(chan struct{})
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", tc.contentType)
						if tc.fixedLength {
							w.Header().Set("Content-Length", strconv.Itoa(len(first)+len(second)))
						}
						w.WriteHeader(status)
						w.(http.Flusher).Flush()
						for _, part := range []struct {
							gate <-chan struct{}
							data string
						}{{allowFirst, first}, {allowSecond, second}, {allowFinish, ""}} {
							select {
							case <-r.Context().Done():
								return
							case <-part.gate:
							}
							if part.data != "" {
								_, _ = io.WriteString(w, part.data)
								w.(http.Flusher).Flush()
							}
						}
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
					server := httptest.NewUnstartedServer(proxy)
					server.EnableHTTP2 = http2
					if http2 {
						server.StartTLS()
					} else {
						server.Start()
					}
					defer server.Close()
					// Cancel the client before closing either server on test failure.
					request, err := http.NewRequest(http.MethodGet, server.URL+"/stream", nil)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(request.Context())
					defer cancel()
					request = request.WithContext(ctx)
					if tc.upgrade {
						request.Header.Set("Connection", "Upgrade")
						request.Header.Set("Upgrade", "websocket")
					}
					progress := make(chan error, 4)
					go func() {
						response, err := server.Client().Do(request)
						if err != nil {
							progress <- err
							return
						}
						defer response.Body.Close()
						if response.StatusCode != status || (response.ProtoMajor == 2) != http2 || response.Header.Get("Content-Type") != tc.contentType {
							progress <- fmt.Errorf("unexpected response: %s %s %v", response.Proto, response.Status, response.Header)
							return
						}
						if tc.fixedLength && response.ContentLength != int64(len(first)+len(second)) {
							progress <- fmt.Errorf("content length changed: %d", response.ContentLength)
							return
						}
						progress <- nil
						for _, want := range []string{first, second} {
							buf := make([]byte, len(want))
							_, err := io.ReadFull(response.Body, buf)
							if err == nil && string(buf) != want {
								err = fmt.Errorf("body = %q, want %q", buf, want)
							}
							progress <- err
							if err != nil {
								return
							}
						}
						rest, err := io.ReadAll(response.Body)
						if err == nil && len(rest) != 0 {
							err = fmt.Errorf("extra response bytes: %q", rest)
						}
						progress <- err
					}()

					awaitStreamResult(t, progress, "headers before first byte")
					close(allowFirst)
					awaitStreamResult(t, progress, "first chunk before second")
					close(allowSecond)
					awaitStreamResult(t, progress, "second chunk before EOF")
					close(allowFinish)
					awaitStreamResult(t, progress, "complete response")
					if capture {
						select {
						case got := <-captured:
							_, body, ok := bytes.Cut(got.body, []byte("\r\n\r\n"))
							if got.err != nil || !ok || string(body) != first+second {
								t.Fatalf("capture = %q, error = %v", got.body, got.err)
							}
						case <-time.After(3 * time.Second):
							t.Fatal("response capture did not complete")
						}
					}
				})
			}
		}
	}
}

func TestReverseProxyBodylessResponse(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, tc := range []struct {
			method string
			status int
		}{{http.MethodHead, http.StatusOK}, {http.MethodGet, http.StatusNoContent}, {http.MethodGet, http.StatusNotModified}} {
			t.Run(fmt.Sprintf("capture=%v/%s/%d", capture, tc.method, tc.status), func(t *testing.T) {
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
				}))
				defer backend.Close()
				var logger Logger = &NoOpLogger{}
				if capture {
					logger = &streamCaptureLogger{response: make(chan streamCapture, 1)}
				}
				proxy := NewProxyServer("")
				if err := proxy.AddRoute("/", backend.URL+"/", logger); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(proxy)
				defer server.Close()
				request, _ := http.NewRequest(tc.method, server.URL+"/", nil)
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || len(body) != 0 || response.StatusCode != tc.status {
					t.Fatalf("status = %d, body = %q, error = %v", response.StatusCode, body, err)
				}
			})
		}
	}
}
