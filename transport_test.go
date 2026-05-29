package loggingproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientProxyConfigDefaultsToEnvironment(t *testing.T) {
	if !(HTTPClientProxyConfig{}).proxyFromEnvironmentEnabled() {
		t.Fatal("expected environment proxy lookup to be enabled by default")
	}
}

func TestHTTPClientProxyConfigCanDisableEnvironment(t *testing.T) {
	disabled := false
	if (HTTPClientProxyConfig{ProxyFromEnvironment: &disabled}).proxyFromEnvironmentEnabled() {
		t.Fatal("expected environment proxy lookup to be disabled")
	}
}

func TestReverseProxyUsesConfiguredHTTPClientProxy(t *testing.T) {
	seenRequests := make(chan string, 1)
	upstreamProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRequests <- r.URL.String()
		_, _ = w.Write([]byte("via upstream proxy"))
	}))
	defer upstreamProxy.Close()

	proxyServer, err := NewProxyServerWithHTTPClientProxy("", HTTPClientProxyConfig{ProxyURL: upstreamProxy.URL})
	if err != nil {
		t.Fatalf("failed to create reverse proxy: %v", err)
	}
	if err := proxyServer.AddRoute("/api/", "http://example.test/base/", &NoOpLogger{}); err != nil {
		t.Fatalf("failed to add route: %v", err)
	}

	testServer := httptest.NewServer(proxyServer)
	defer testServer.Close()

	resp, err := http.Get(testServer.URL + "/api/widgets?x=1")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if string(body) != "via upstream proxy" {
		t.Fatalf("expected upstream proxy response, got %q", string(body))
	}

	select {
	case seenURL := <-seenRequests:
		expectedURL := "http://example.test/base/widgets?x=1"
		if seenURL != expectedURL {
			t.Fatalf("expected upstream proxy to receive %q, got %q", expectedURL, seenURL)
		}
	default:
		t.Fatal("upstream proxy did not receive the request")
	}
}

func TestReverseProxyUsesHTTPProxyFromEnvironment(t *testing.T) {
	seenRequests := make(chan string, 1)
	upstreamProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRequests <- r.URL.String()
		_, _ = w.Write([]byte("via environment proxy"))
	}))
	defer upstreamProxy.Close()

	t.Setenv("HTTP_PROXY", upstreamProxy.URL)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "no-match.invalid")
	t.Setenv("REQUEST_METHOD", "")

	proxyServer, err := NewProxyServerWithHTTPClientProxy("", HTTPClientProxyConfig{})
	if err != nil {
		t.Fatalf("failed to create reverse proxy: %v", err)
	}
	if err := proxyServer.AddRoute("/api/", "http://example.test/base/", &NoOpLogger{}); err != nil {
		t.Fatalf("failed to add route: %v", err)
	}

	testServer := httptest.NewServer(proxyServer)
	defer testServer.Close()

	resp, err := http.Get(testServer.URL + "/api/widgets?x=1")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	if string(body) != "via environment proxy" {
		t.Fatalf("expected environment proxy response, got %q", string(body))
	}

	select {
	case seenURL := <-seenRequests:
		expectedURL := "http://example.test/base/widgets?x=1"
		if seenURL != expectedURL {
			t.Fatalf("expected environment proxy to receive %q, got %q", expectedURL, seenURL)
		}
	default:
		t.Fatal("environment proxy did not receive the request")
	}
}
