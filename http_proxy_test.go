package loggingproxy

import (
	"crypto/tls"
	"crypto/x509"
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
