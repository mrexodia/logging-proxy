package loggingproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newProxyClient(t *testing.T, proxyURL string, tlsConfig *tls.Config) *http.Client {
	t.Helper()

	parsedProxyURL, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("failed to parse proxy URL: %v", err)
	}

	transport := newDirectTransport()
	transport.Proxy = http.ProxyURL(parsedProxyURL)
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig.Clone()
	}

	return &http.Client{Transport: transport}
}

func proxyURLWithUser(t *testing.T, rawURL, username, password string) string {
	t.Helper()

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("failed to parse proxy URL: %v", err)
	}
	parsedURL.User = url.UserPassword(username, password)
	return parsedURL.String()
}

type abandoningLogger struct{}

func (abandoningLogger) LogRequest(_ RequestMetadata, _ time.Time, rawRequestStream io.ReadCloser) {
	_ = rawRequestStream.Close()
}

func (abandoningLogger) LogResponse(_ RequestMetadata, _ time.Time, rawResponseStream io.ReadCloser) {
	_ = rawResponseStream.Close()
}

func TestHTTPProxyServerForwardsHTTPRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		defer r.Body.Close()

		fmt.Fprintf(w, "%s %s?%s %s", r.Method, r.URL.Path, r.URL.RawQuery, string(body))
	}))
	defer backend.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{Logger: &NoOpLogger{}})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	client := newProxyClient(t, proxy.URL, nil)

	request, err := http.NewRequest(http.MethodPost, backend.URL+"/api/test?hello=world", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	request.Header.Set("Content-Type", "text/plain")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.StatusCode)
	}

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	expected := "POST /api/test?hello=world payload"
	if string(responseBody) != expected {
		t.Fatalf("expected response %q, got %q", expected, string(responseBody))
	}
}

func TestHTTPProxyServerRequiresBasicAuthForHTTPRequests(t *testing.T) {
	seenProxyAuthorization := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenProxyAuthorization <- r.Header.Get("Proxy-Authorization")
		fmt.Fprint(w, "authorized")
	}))
	defer backend.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger: &NoOpLogger{},
		Auth: HTTPProxyAuthConfig{
			Username: "user",
			Password: "pass",
		},
	})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	unauthorizedClient := newProxyClient(t, proxy.URL, nil)
	unauthorizedResponse, err := unauthorizedClient.Get(backend.URL + "/blocked")
	if err != nil {
		t.Fatalf("unauthorized proxy request failed: %v", err)
	}
	defer unauthorizedResponse.Body.Close()
	if unauthorizedResponse.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("expected unauthorized status 407, got %d", unauthorizedResponse.StatusCode)
	}
	if got := unauthorizedResponse.Header.Get("Proxy-Authenticate"); got != `Basic realm="logging-proxy"` {
		t.Fatalf("expected Proxy-Authenticate challenge, got %q", got)
	}
	select {
	case <-seenProxyAuthorization:
		t.Fatal("backend received unauthorized request")
	default:
	}

	authorizedClient := newProxyClient(t, proxyURLWithUser(t, proxy.URL, "user", "pass"), nil)
	authorizedResponse, err := authorizedClient.Get(backend.URL + "/allowed")
	if err != nil {
		t.Fatalf("authorized proxy request failed: %v", err)
	}
	defer authorizedResponse.Body.Close()
	if authorizedResponse.StatusCode != http.StatusOK {
		t.Fatalf("expected authorized status 200, got %d", authorizedResponse.StatusCode)
	}
	body, err := io.ReadAll(authorizedResponse.Body)
	if err != nil {
		t.Fatalf("failed to read authorized response: %v", err)
	}
	if string(body) != "authorized" {
		t.Fatalf("expected authorized response body, got %q", string(body))
	}

	select {
	case proxyAuthorization := <-seenProxyAuthorization:
		if proxyAuthorization != "" {
			t.Fatalf("proxy authorization header leaked upstream: %q", proxyAuthorization)
		}
	case <-time.After(time.Second):
		t.Fatal("backend did not receive authorized request")
	}
}

func TestHTTPProxyServerRequiresBasicAuthForConnect(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "connect authorized")
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("failed to parse backend URL: %v", err)
	}
	backendHost, _, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("failed to split backend host: %v", err)
	}

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger: &NoOpLogger{},
		Auth: HTTPProxyAuthConfig{
			Username: "user",
			Password: "pass",
		},
	})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("failed to parse proxy URL: %v", err)
	}

	unauthorizedConn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	if _, err := fmt.Fprintf(unauthorizedConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", backendURL.Host, backendURL.Host); err != nil {
		t.Fatalf("failed to write unauthorized CONNECT: %v", err)
	}
	unauthorizedReader := bufio.NewReader(unauthorizedConn)
	unauthorizedStatus, err := unauthorizedReader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read unauthorized CONNECT response: %v", err)
	}
	if !strings.Contains(unauthorizedStatus, "407") {
		t.Fatalf("expected CONNECT 407, got %q", unauthorizedStatus)
	}
	var unauthorizedHeaders strings.Builder
	for {
		line, err := unauthorizedReader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read unauthorized CONNECT headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
		unauthorizedHeaders.WriteString(line)
	}
	if !strings.Contains(unauthorizedHeaders.String(), `Proxy-Authenticate: Basic realm="logging-proxy"`) {
		t.Fatalf("expected CONNECT auth challenge, got %q", unauthorizedHeaders.String())
	}
	unauthorizedConn.Close()

	authorizedConn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer authorizedConn.Close()
	token := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if _, err := fmt.Fprintf(authorizedConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", backendURL.Host, backendURL.Host, token); err != nil {
		t.Fatalf("failed to write authorized CONNECT: %v", err)
	}
	authorizedReader := bufio.NewReader(authorizedConn)
	authorizedStatus, err := authorizedReader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read authorized CONNECT response: %v", err)
	}
	if !strings.Contains(authorizedStatus, "200") {
		t.Fatalf("expected CONNECT 200, got %q", authorizedStatus)
	}
	for {
		line, err := authorizedReader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read authorized CONNECT headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(backend.Certificate())
	tlsConn := tls.Client(authorizedConn, &tls.Config{RootCAs: clientRoots, ServerName: backendHost})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("failed CONNECT TLS handshake: %v", err)
	}
	defer tlsConn.Close()

	if _, err := fmt.Fprintf(tlsConn, "GET /secure HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", backendURL.Host); err != nil {
		t.Fatalf("failed to write tunneled request: %v", err)
	}
	tunneledStatus, err := bufio.NewReader(tlsConn).ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read tunneled response: %v", err)
	}
	if !strings.HasPrefix(tunneledStatus, "HTTP/1.1 200") {
		t.Fatalf("expected tunneled status 200, got %q", tunneledStatus)
	}
}

func TestHTTPProxyServerRejectsPartialBasicAuthConfig(t *testing.T) {
	_, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger: &NoOpLogger{},
		Auth:   HTTPProxyAuthConfig{Username: "user"},
	})
	if err == nil {
		t.Fatal("expected partial proxy auth config to fail")
	}
}

func TestHTTPProxyServerPreservesAcceptEncodingBeforeForwarding(t *testing.T) {
	seenAcceptEncoding := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAcceptEncoding <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer backend.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{Logger: &NoOpLogger{}})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	client := newProxyClient(t, proxy.URL, nil)
	request, err := http.NewRequest(http.MethodGet, backend.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if !strings.Contains(string(responseBody), "message_stop") {
		t.Fatalf("expected complete streaming response, got %q", string(responseBody))
	}

	select {
	case acceptEncoding := <-seenAcceptEncoding:
		expected := "gzip, deflate, br, zstd"
		if acceptEncoding != expected {
			t.Fatalf("expected proxy to preserve Accept-Encoding %q, got %q", expected, acceptEncoding)
		}
	case <-time.After(time.Second):
		t.Fatal("backend did not receive request")
	}
}

func TestHTTPProxyServerLoggingReaderCloseDoesNotBreakProxying(t *testing.T) {
	const responseBody = "0123456789abcdef"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		for i := 0; i < 4096; i++ {
			fmt.Fprint(w, responseBody)
		}
	}))
	defer backend.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{Logger: abandoningLogger{}})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	client := newProxyClient(t, proxy.URL, nil)
	response, err := client.Get(backend.URL + "/large")
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	expected := strings.Repeat(responseBody, 4096)
	if string(body) != expected {
		t.Fatalf("expected full proxied body length %d, got %d", len(expected), len(body))
	}
}

func TestHTTPProxyServerUsesConfiguredUpstreamProxyForHTTP(t *testing.T) {
	seenRequests := make(chan string, 1)
	upstreamProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRequests <- r.URL.String()
		_, _ = w.Write([]byte("via upstream proxy"))
	}))
	defer upstreamProxy.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:      &NoOpLogger{},
		ClientProxy: HTTPClientProxyConfig{ProxyURL: upstreamProxy.URL},
	})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	client := newProxyClient(t, proxy.URL, nil)
	response, err := client.Get("http://example.test/api/test?hello=world")
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if string(responseBody) != "via upstream proxy" {
		t.Fatalf("expected upstream proxy response, got %q", string(responseBody))
	}

	select {
	case seenURL := <-seenRequests:
		expectedURL := "http://example.test/api/test?hello=world"
		if seenURL != expectedURL {
			t.Fatalf("expected upstream proxy to receive %q, got %q", expectedURL, seenURL)
		}
	default:
		t.Fatal("upstream proxy did not receive the request")
	}
}

func TestHTTPProxyServerSupportsConnectTunnels(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "secure %s", r.URL.Path)
	}))
	defer backend.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{Logger: &NoOpLogger{}})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	client := newProxyClient(t, proxy.URL, &tls.Config{InsecureSkipVerify: true})

	response, err := client.Get(backend.URL + "/secret")
	if err != nil {
		t.Fatalf("proxy CONNECT request failed: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.StatusCode)
	}

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	expected := "secure /secret"
	if string(responseBody) != expected {
		t.Fatalf("expected response %q, got %q", expected, string(responseBody))
	}
}

func TestHTTPProxyServerUsesConfiguredUpstreamProxyForConnect(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "secure %s", r.URL.Path)
	}))
	defer backend.Close()

	seenConnects := make(chan string, 1)
	upstreamProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Fatalf("expected CONNECT request, got %s", r.Method)
		}
		seenConnects <- r.Host

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("upstream proxy response writer does not support hijacking")
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("failed to hijack upstream proxy connection: %v", err)
		}

		targetConn, err := net.Dial("tcp", r.Host)
		if err != nil {
			_, _ = io.WriteString(clientConn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			_ = clientConn.Close()
			return
		}
		_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go copyAndCloseConn(targetConn, clientConn)
		go copyAndCloseConn(clientConn, targetConn)
	}))
	defer upstreamProxy.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:      &NoOpLogger{},
		ClientProxy: HTTPClientProxyConfig{ProxyURL: upstreamProxy.URL},
	})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	client := newProxyClient(t, proxy.URL, &tls.Config{InsecureSkipVerify: true})
	response, err := client.Get(backend.URL + "/secret")
	if err != nil {
		t.Fatalf("proxy CONNECT request failed: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if string(responseBody) != "secure /secret" {
		t.Fatalf("expected response %q, got %q", "secure /secret", string(responseBody))
	}

	select {
	case seenHost := <-seenConnects:
		expectedHost := strings.TrimPrefix(backend.URL, "https://")
		if seenHost != expectedHost {
			t.Fatalf("expected upstream proxy CONNECT host %q, got %q", expectedHost, seenHost)
		}
	default:
		t.Fatal("upstream proxy did not receive CONNECT")
	}
}

func TestHTTPProxyServerUsesConfiguredSOCKSProxyForConnect(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "secure %s", r.URL.Path)
	}))
	defer backend.Close()

	seenTargets := make(chan string, 1)
	socksProxyAddr := startTestSOCKS5Proxy(t, seenTargets)

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:      &NoOpLogger{},
		ClientProxy: HTTPClientProxyConfig{ProxyURL: "socks5://" + socksProxyAddr},
	})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	client := newProxyClient(t, proxy.URL, &tls.Config{InsecureSkipVerify: true})
	response, err := client.Get(backend.URL + "/secret")
	if err != nil {
		t.Fatalf("proxy CONNECT request failed: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if string(responseBody) != "secure /secret" {
		t.Fatalf("expected response %q, got %q", "secure /secret", string(responseBody))
	}

	select {
	case seenTarget := <-seenTargets:
		expectedTarget := strings.TrimPrefix(backend.URL, "https://")
		if seenTarget != expectedTarget {
			t.Fatalf("expected SOCKS target %q, got %q", expectedTarget, seenTarget)
		}
	default:
		t.Fatal("SOCKS proxy did not receive CONNECT target")
	}
}

func startTestSOCKS5Proxy(t *testing.T, seenTargets chan<- string) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start SOCKS5 proxy: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleTestSOCKS5Conn(conn, seenTargets)
		}
	}()

	return listener.Addr().String()
}

func handleTestSOCKS5Conn(conn net.Conn, seenTargets chan<- string) {
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		_ = conn.Close()
		return
	}
	if greeting[0] != 5 {
		_ = conn.Close()
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		_ = conn.Close()
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		_ = conn.Close()
		return
	}

	requestHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, requestHeader); err != nil {
		_ = conn.Close()
		return
	}
	if requestHeader[0] != 5 || requestHeader[1] != 1 {
		writeTestSOCKS5Response(conn, 7)
		_ = conn.Close()
		return
	}

	host, err := readTestSOCKS5Address(conn, requestHeader[3])
	if err != nil {
		writeTestSOCKS5Response(conn, 8)
		_ = conn.Close()
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		_ = conn.Close()
		return
	}
	port := binary.BigEndian.Uint16(portBytes)
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))
	seenTargets <- target

	targetConn, err := net.Dial("tcp", target)
	if err != nil {
		writeTestSOCKS5Response(conn, 5)
		_ = conn.Close()
		return
	}
	writeTestSOCKS5Response(conn, 0)
	go copyAndCloseConn(targetConn, conn)
	copyAndCloseConn(conn, targetConn)
}

func readTestSOCKS5Address(reader io.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		addr := make([]byte, net.IPv4len)
		_, err := io.ReadFull(reader, addr)
		return net.IP(addr).String(), err
	case 3:
		length := []byte{0}
		if _, err := io.ReadFull(reader, length); err != nil {
			return "", err
		}
		addr := make([]byte, int(length[0]))
		_, err := io.ReadFull(reader, addr)
		return string(addr), err
	case 4:
		addr := make([]byte, net.IPv6len)
		_, err := io.ReadFull(reader, addr)
		return net.IP(addr).String(), err
	default:
		return "", fmt.Errorf("unsupported SOCKS5 address type %d", addressType)
	}
}

func writeTestSOCKS5Response(writer io.Writer, status byte) {
	_, _ = writer.Write([]byte{5, status, 0, 1, 0, 0, 0, 0, 0, 0})
}

func copyAndCloseConn(dst net.Conn, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

func TestHTTPProxyServerMITMExcludeHostsTunnelsWithoutLogging(t *testing.T) {
	logDir := t.TempDir()
	fileLogger, err := NewFileLogger(logDir, false)
	if err != nil {
		t.Fatalf("failed to create file logger: %v", err)
	}

	ca, err := LoadOrCreateMITMCA(MITMCAConfig{
		CertFile: filepath.Join(logDir, "mitm-ca-cert.pem"),
		KeyFile:  filepath.Join(logDir, "mitm-ca-key.pem"),
	})
	if err != nil {
		t.Fatalf("failed to create MITM CA: %v", err)
	}

	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read HTTPS request body: %v", err)
		}
		defer r.Body.Close()

		fmt.Fprintf(w, "excluded tunnel received %s", string(body))
	}))
	defer backend.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:           fileLogger,
		MITM:             true,
		MITMCertificate:  ca,
		MITMExcludeHosts: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("failed to create MITM proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(backend.Certificate())
	client := newProxyClient(t, proxy.URL, &tls.Config{RootCAs: clientRoots})

	requestBody := `{"prompt":"excluded secret"}`
	request, err := http.NewRequest(http.MethodPost, backend.URL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("failed to create excluded MITM request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("excluded tunnel request failed: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read excluded tunnel response: %v", err)
	}
	if !strings.Contains(string(responseBody), "excluded secret") {
		t.Fatalf("expected excluded tunnel response to contain request body, got %q", string(responseBody))
	}

	time.Sleep(200 * time.Millisecond)

	files, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("failed to read log directory: %v", err)
	}
	for _, file := range files {
		if strings.Contains(file.Name(), "request.bin") || strings.Contains(file.Name(), "response.bin") {
			t.Fatalf("expected excluded MITM host to skip plaintext logging, found %s", file.Name())
		}
	}
}

func TestHTTPProxyServerMITMExcludeHostsLogsConnectToConsoleOnly(t *testing.T) {
	logDir := t.TempDir()
	fileLogger, err := NewFileLogger(logDir, true)
	if err != nil {
		t.Fatalf("failed to create file logger: %v", err)
	}

	ca, err := LoadOrCreateMITMCA(MITMCAConfig{
		CertFile: filepath.Join(logDir, "mitm-ca-cert.pem"),
		KeyFile:  filepath.Join(logDir, "mitm-ca-key.pem"),
	})
	if err != nil {
		t.Fatalf("failed to create MITM CA: %v", err)
	}

	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "excluded ok")
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("failed to parse backend URL: %v", err)
	}

	var console bytes.Buffer
	oldOutput := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&console)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
	}()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:           fileLogger,
		MITM:             true,
		MITMCertificate:  ca,
		MITMExcludeHosts: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("failed to create MITM proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(backend.Certificate())
	client := newProxyClient(t, proxy.URL, &tls.Config{RootCAs: clientRoots})

	response, err := client.Get(backend.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("excluded tunnel request failed: %v", err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("failed to read excluded tunnel response: %v", err)
	}

	output := console.String()
	expectedConnect := "CONNECT " + backendURL.Host
	if !strings.Contains(output, expectedConnect) {
		t.Fatalf("expected console output to contain %q, got %q", expectedConnect, output)
	}
	if strings.Contains(output, expectedConnect+" ->") {
		t.Fatalf("expected CONNECT console output to omit redundant destination, got %q", output)
	}

	files, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("failed to read log directory: %v", err)
	}
	for _, file := range files {
		if strings.Contains(file.Name(), "_request") || strings.Contains(file.Name(), "_response") {
			t.Fatalf("expected excluded MITM host to skip disk logging, found %s", file.Name())
		}
	}
}

func TestHTTPProxyServerMITMIncludeHostsTunnelsNonMatchingWithoutLogging(t *testing.T) {
	logDir := t.TempDir()
	fileLogger, err := NewFileLogger(logDir, false)
	if err != nil {
		t.Fatalf("failed to create file logger: %v", err)
	}

	ca, err := LoadOrCreateMITMCA(MITMCAConfig{
		CertFile: filepath.Join(logDir, "mitm-ca-cert.pem"),
		KeyFile:  filepath.Join(logDir, "mitm-ca-key.pem"),
	})
	if err != nil {
		t.Fatalf("failed to create MITM CA: %v", err)
	}

	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read HTTPS request body: %v", err)
		}
		defer r.Body.Close()

		fmt.Fprintf(w, "non-included tunnel received %s", string(body))
	}))
	defer backend.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:           fileLogger,
		MITM:             true,
		MITMCertificate:  ca,
		MITMIncludeHosts: []string{"api.anthropic.com"},
	})
	if err != nil {
		t.Fatalf("failed to create MITM proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(backend.Certificate())
	client := newProxyClient(t, proxy.URL, &tls.Config{RootCAs: clientRoots})

	requestBody := `{"prompt":"non-included secret"}`
	request, err := http.NewRequest(http.MethodPost, backend.URL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("failed to create non-included MITM request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("non-included tunnel request failed: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read non-included tunnel response: %v", err)
	}
	if !strings.Contains(string(responseBody), "non-included secret") {
		t.Fatalf("expected non-included tunnel response to contain request body, got %q", string(responseBody))
	}

	time.Sleep(200 * time.Millisecond)

	files, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("failed to read log directory: %v", err)
	}
	for _, file := range files {
		if strings.Contains(file.Name(), "request.bin") || strings.Contains(file.Name(), "response.bin") {
			t.Fatalf("expected non-included MITM host to skip plaintext logging, found %s", file.Name())
		}
	}
}

func TestHTTPProxyServerPlainHTTPIncludeHostsSkipsNonMatchingLogs(t *testing.T) {
	logDir := t.TempDir()
	fileLogger, err := NewFileLogger(logDir, false)
	if err != nil {
		t.Fatalf("failed to create file logger: %v", err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "plain ok")
	}))
	defer backend.Close()

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:           fileLogger,
		MITMIncludeHosts: []string{"api.anthropic.com"},
	})
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	client := newProxyClient(t, proxy.URL, nil)
	response, err := client.Get(backend.URL + "/plain")
	if err != nil {
		t.Fatalf("plain HTTP proxy request failed: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if string(body) != "plain ok" {
		t.Fatalf("expected plain response, got %q", string(body))
	}

	time.Sleep(200 * time.Millisecond)

	files, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("failed to read log directory: %v", err)
	}
	for _, file := range files {
		if strings.Contains(file.Name(), "request.bin") || strings.Contains(file.Name(), "response.bin") {
			t.Fatalf("expected non-included plain HTTP host to skip logging, found %s", file.Name())
		}
	}
}

func TestHTTPProxyServerMITMWritesClientHTTPVersion(t *testing.T) {
	logDir := t.TempDir()
	ca, err := LoadOrCreateMITMCA(MITMCAConfig{
		CertFile: filepath.Join(logDir, "mitm-ca-cert.pem"),
		KeyFile:  filepath.Join(logDir, "mitm-ca-key.pem"),
	})
	if err != nil {
		t.Fatalf("failed to create MITM CA: %v", err)
	}

	upstreamProto := make(chan string, 1)
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamProto <- r.Proto
		fmt.Fprint(w, "ok")
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("failed to parse backend URL: %v", err)
	}
	backendHost, _, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("failed to split backend host: %v", err)
	}

	upstreamRoots := x509.NewCertPool()
	upstreamRoots.AddCert(backend.Certificate())

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:          &NoOpLogger{},
		MITM:            true,
		MITMCertificate: ca,
		UpstreamTLSConfig: &tls.Config{
			RootCAs: upstreamRoots,
		},
	})
	if err != nil {
		t.Fatalf("failed to create MITM proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("failed to parse proxy URL: %v", err)
	}

	conn, err := net.Dial("tcp", proxyURL.Host)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", backendURL.Host, backendURL.Host); err != nil {
		t.Fatalf("failed to write CONNECT: %v", err)
	}
	connectReader := bufio.NewReader(conn)
	connectStatus, err := connectReader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read CONNECT response: %v", err)
	}
	if !strings.Contains(connectStatus, "200") {
		t.Fatalf("expected CONNECT 200, got %q", connectStatus)
	}
	for {
		line, err := connectReader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read CONNECT headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(ca.Leaf)
	tlsConn := tls.Client(conn, &tls.Config{RootCAs: clientRoots, ServerName: backendHost})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("failed MITM TLS handshake: %v", err)
	}
	defer tlsConn.Close()

	if _, err := fmt.Fprintf(tlsConn, "GET /proto HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", backendURL.Host); err != nil {
		t.Fatalf("failed to write MITM request: %v", err)
	}
	statusLine, err := bufio.NewReader(tlsConn).ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read MITM response status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		t.Fatalf("expected MITM response to use client HTTP/1.1 status line, got %q", statusLine)
	}

	select {
	case proto := <-upstreamProto:
		if proto != "HTTP/2.0" {
			t.Fatalf("expected upstream request to use HTTP/2.0, got %q", proto)
		}
	case <-time.After(time.Second):
		t.Fatal("backend did not receive request")
	}
}

func TestHTTPProxyServerMITMLogsHTTPSBodies(t *testing.T) {
	logDir := t.TempDir()
	fileLogger, err := NewFileLogger(logDir, false)
	if err != nil {
		t.Fatalf("failed to create file logger: %v", err)
	}

	ca, err := LoadOrCreateMITMCA(MITMCAConfig{
		CertFile: filepath.Join(logDir, "mitm-ca-cert.pem"),
		KeyFile:  filepath.Join(logDir, "mitm-ca-key.pem"),
	})
	if err != nil {
		t.Fatalf("failed to create MITM CA: %v", err)
	}

	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read HTTPS request body: %v", err)
		}
		defer r.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"received":%q}`, string(body))
	}))
	defer backend.Close()

	upstreamRoots := x509.NewCertPool()
	upstreamRoots.AddCert(backend.Certificate())

	proxyHandler, err := NewHTTPProxyServer(HTTPProxyOptions{
		Logger:          fileLogger,
		MITM:            true,
		MITMCertificate: ca,
		UpstreamTLSConfig: &tls.Config{
			RootCAs: upstreamRoots,
		},
	})
	if err != nil {
		t.Fatalf("failed to create MITM proxy: %v", err)
	}
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(ca.Leaf)
	client := newProxyClient(t, proxy.URL, &tls.Config{RootCAs: clientRoots})

	requestBody := `{"prompt":"hello claude"}`
	request, err := http.NewRequest(http.MethodPost, backend.URL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("failed to create MITM request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("MITM proxy request failed: %v", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("failed to read MITM response: %v", err)
	}
	if !strings.Contains(string(responseBody), "hello claude") {
		t.Fatalf("expected proxied response to contain request body, got %q", string(responseBody))
	}

	time.Sleep(200 * time.Millisecond)

	files, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("failed to read log directory: %v", err)
	}

	var requestLog string
	var responseLog string
	for _, file := range files {
		if strings.Contains(file.Name(), "request.bin") {
			data, err := os.ReadFile(filepath.Join(logDir, file.Name()))
			if err != nil {
				t.Fatalf("failed to read request log: %v", err)
			}
			requestLog = string(data)
		}
		if strings.Contains(file.Name(), "response.bin") {
			data, err := os.ReadFile(filepath.Join(logDir, file.Name()))
			if err != nil {
				t.Fatalf("failed to read response log: %v", err)
			}
			responseLog = string(data)
		}
	}

	if !strings.Contains(requestLog, "hello claude") {
		t.Fatalf("expected HTTPS request log to contain decrypted body, got %q", requestLog)
	}
	if !strings.Contains(responseLog, "hello claude") {
		t.Fatalf("expected HTTPS response log to contain decrypted body, got %q", responseLog)
	}
}
