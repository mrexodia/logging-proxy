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

func TestHTTPTransportUsesConcurrencyFriendlyIdlePool(t *testing.T) {
	transport := cloneDefaultTransport()
	if transport.MaxIdleConns != defaultMaxIdleConns {
		t.Fatalf("MaxIdleConns = %d, want %d", transport.MaxIdleConns, defaultMaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != defaultMaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want %d", transport.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
	}
}

func clearProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy",
		"NO_PROXY", "no_proxy",
		"REQUEST_METHOD",
	} {
		t.Setenv(name, "")
	}
}

func TestAllProxyFillsMissingSchemeSpecificProxies(t *testing.T) {
	clearProxyEnvironment(t)
	t.Setenv("ALL_PROXY", "socks5://proxy.example:1080")

	environment := ReadHTTPClientProxyEnvironment()
	if environment.HTTPProxy != "socks5://proxy.example:1080" {
		t.Fatalf("HTTP proxy = %q, want ALL_PROXY fallback", environment.HTTPProxy)
	}
	if environment.HTTPSProxy != "socks5://proxy.example:1080" {
		t.Fatalf("HTTPS proxy = %q, want ALL_PROXY fallback", environment.HTTPSProxy)
	}
}

func TestSchemeSpecificProxyOverridesLowercaseAllProxy(t *testing.T) {
	clearProxyEnvironment(t)
	t.Setenv("HTTP_PROXY", "http://http-proxy.example:3128")
	t.Setenv("all_proxy", "socks5://fallback.example:1080")

	environment := ReadHTTPClientProxyEnvironment()
	if environment.HTTPProxy != "http://http-proxy.example:3128" {
		t.Fatalf("HTTP proxy = %q, want scheme-specific proxy", environment.HTTPProxy)
	}
	if environment.HTTPSProxy != "socks5://fallback.example:1080" {
		t.Fatalf("HTTPS proxy = %q, want lowercase all_proxy fallback", environment.HTTPSProxy)
	}
}

func TestAllProxyHonorsNoProxy(t *testing.T) {
	clearProxyEnvironment(t)
	t.Setenv("ALL_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "bypass.example")

	request, err := http.NewRequest(http.MethodGet, "https://bypass.example/resource", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	proxyURL, err := proxyFromEnvironment(request)
	if err != nil {
		t.Fatalf("proxy lookup failed: %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("NO_PROXY destination selected proxy %q", proxyURL)
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
