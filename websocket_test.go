package loggingproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebSocketForwardingAndExactCapture(t *testing.T) {
	for _, mode := range []string{"reverse", "forward", "mitm", "reverse-disabled", "forward-disabled", "reverse-abandoning", "forward-abandoning"} {
		t.Run(mode, func(t *testing.T) {
			clientFrame := []byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2}
			serverFrame := []byte{0x81, 2, 'h', 'i'}
			backendErrors := make(chan error, 1)
			backendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, buffer, err := http.NewResponseController(w).Hijack()
				if err != nil {
					backendErrors <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, err = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: test\r\n\r\n")
				if err != nil {
					backendErrors <- err
					return
				}
				_ = buffer.Flush()
				got := make([]byte, len(clientFrame))
				_, err = io.ReadFull(buffer, got)
				if err != nil {
					backendErrors <- err
					return
				}
				if !bytes.Equal(got, clientFrame) {
					backendErrors <- fmt.Errorf("client frame changed: %x", got)
					return
				}
				_, err = conn.Write(serverFrame)
				backendErrors <- err
			})
			var backend *httptest.Server
			if mode == "mitm" {
				backend = httptest.NewTLSServer(backendHandler)
			} else {
				backend = httptest.NewServer(backendHandler)
			}
			defer backend.Close()
			dir := t.TempDir()
			fileLogger, err := NewFileLogger(dir, false)
			if err != nil {
				t.Fatal(err)
			}
			var logger Logger = fileLogger
			if strings.HasSuffix(mode, "disabled") {
				logger = &NoOpLogger{}
			}
			if strings.HasSuffix(mode, "abandoning") {
				logger = abandoningLogger{}
			}
			var handler http.Handler
			var ca *MITMCA
			if strings.HasPrefix(mode, "reverse") {
				proxy := NewProxyServer("")
				if err = proxy.AddRoute("/", backend.URL+"/", logger); err != nil {
					t.Fatal(err)
				}
				handler = proxy
			} else {
				fromEnvironment := false
				options := HTTPProxyOptions{Logger: logger, ClientProxy: HTTPClientProxyConfig{ProxyFromEnvironment: &fromEnvironment}}
				if mode == "mitm" {
					ca = testMITMCA(t, dir)
					options.MITM = true
					options.MITMCA = ca
					pool := x509.NewCertPool()
					pool.AddCert(backend.Certificate())
					options.UpstreamTLSConfig = &tls.Config{RootCAs: pool}
				}
				handler, err = NewHTTPProxyServer(options)
				if err != nil {
					t.Fatal(err)
				}
			}
			proxyServer := httptest.NewServer(handler)
			defer proxyServer.Close()
			proxyURL, _ := url.Parse(proxyServer.URL)
			backendURL, _ := url.Parse(backend.URL)
			conn, err := net.DialTimeout("tcp", proxyURL.Host, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			target := "/stream"
			if strings.HasPrefix(mode, "forward") {
				target = backend.URL + "/stream"
			}
			if mode == "mitm" {
				_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", backendURL.Host, backendURL.Host)
				response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 {
					t.Fatal(response.Status)
				}
				roots := x509.NewCertPool()
				pem, err := os.ReadFile(filepath.Join(dir, "certs", "root-ca.crt"))
				if err != nil {
					t.Fatal(err)
				}
				if !roots.AppendCertsFromPEM(pem) {
					t.Fatal("invalid CA")
				}
				tlsConn := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: backendURL.Hostname()})
				if err = tlsConn.Handshake(); err != nil {
					t.Fatal(err)
				}
				conn = tlsConn
			}
			_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", target, backendURL.Host)
			if err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			response, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 101 {
				t.Fatal(response.Status)
			}
			if _, err = conn.Write(clientFrame); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(serverFrame))
			if _, err = io.ReadFull(reader, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, serverFrame) {
				t.Fatalf("server frame changed: %x", got)
			}
			_ = conn.Close()
			if err = <-backendErrors; err != nil {
				t.Fatal(err)
			}
			if strings.Contains(mode, "disabled") || strings.Contains(mode, "abandoning") {
				entries, _ := filepath.Glob(filepath.Join(dir, "*", "*.bin"))
				if len(entries) != 0 {
					t.Fatal("disabled logger wrote captures")
				}
				return
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				files, _ := filepath.Glob(filepath.Join(dir, "*", "*_metadata.jsonl"))
				if len(files) == 1 {
					data, _ := os.ReadFile(files[0])
					if bytes.Contains(data, []byte(`"event":"request_completed"`)) && bytes.Contains(data, []byte(`"event":"response_completed"`)) {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("WebSocket capture did not complete")
				}
				time.Sleep(10 * time.Millisecond)
			}
			for direction, frame := range map[string][]byte{"request": clientFrame, "response": serverFrame} {
				files, _ := filepath.Glob(filepath.Join(dir, "*", "*_"+direction+".bin"))
				if len(files) != 1 {
					t.Fatalf("unexpected %s file count: %d", direction, len(files))
				}
				data, _ := os.ReadFile(files[0])
				parts := bytes.SplitN(data, []byte("\r\n\r\n"), 2)
				if len(parts) != 2 || !bytes.Equal(parts[1], frame) {
					t.Fatalf("%s capture not exact: %x", direction, data)
				}
			}
		})
	}
}
