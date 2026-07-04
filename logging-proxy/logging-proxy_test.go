package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	loggingproxy "github.com/mrexodia/logging-proxy"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return path
}

func TestCRLHostnameUsesConfiguredMITMHostname(t *testing.T) {
	got, err := crlHostname("litellm.ogilvie.lan", "0.0.0.0", 8841)
	if err != nil {
		t.Fatalf("crlHostname failed: %v", err)
	}
	want := "litellm.ogilvie.lan:8841"
	if got != want {
		t.Fatalf("crlHostname() = %q, want %q", got, want)
	}
}

func TestCRLHostnameFallsBackToConcreteListenHost(t *testing.T) {
	got, err := crlHostname("", "proxy.example", 8841)
	if err != nil {
		t.Fatalf("crlHostname failed: %v", err)
	}
	want := "proxy.example:8841"
	if got != want {
		t.Fatalf("crlHostname() = %q, want %q", got, want)
	}
}

func TestCRLHostnameRejectsWildcardListenHostWithoutMITMHostname(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", ""} {
		_, err := crlHostname("", host, 8841)
		if err == nil {
			t.Fatalf("expected crlHostname to reject wildcard host %q", host)
		}
		if !strings.Contains(err.Error(), "proxy.mitm.hostname is required") {
			t.Fatalf("unexpected error for host %q: %v", host, err)
		}
	}
}

func TestCRLHostnameRejectsWildcardMITMHostnameWithPort(t *testing.T) {
	for _, host := range []string{"0.0.0.0:8841", "[::]:8841"} {
		_, err := crlHostname(host, "127.0.0.1", 8841)
		if err == nil {
			t.Fatalf("expected crlHostname to reject wildcard MITM hostname %q", host)
		}
		if !strings.Contains(err.Error(), "invalid proxy.mitm.hostname") {
			t.Fatalf("unexpected error for host %q: %v", host, err)
		}
	}
}

func TestDescribeHTTPClientProxyConfigExplicitProxy(t *testing.T) {
	endpoints, message, err := describeHTTPClientProxyConfig(loggingproxy.HTTPClientProxyConfig{
		ProxyURL: "http://user:pass@proxy.example:3128",
	})
	if err != nil {
		t.Fatalf("describeHTTPClientProxyConfig failed: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("expected one proxy endpoint, got %d", len(endpoints))
	}
	if endpoints[0].label != "http_client.proxy_url" {
		t.Fatalf("unexpected endpoint label %q", endpoints[0].label)
	}
	if endpoints[0].url.Host != "proxy.example:3128" {
		t.Fatalf("unexpected endpoint URL %q", endpoints[0].url.String())
	}
	if !strings.Contains(message, "http_client.proxy_url") || !strings.Contains(message, "proxy.example:3128") {
		t.Fatalf("unexpected proxy log message %q", message)
	}
	if strings.Contains(message, "pass") {
		t.Fatalf("proxy log message leaked password: %q", message)
	}
}

func TestValidateHTTPClientProxyRejectsSelfProxyOnWildcardListener(t *testing.T) {
	proxyURL, err := loggingproxy.ParseHTTPClientProxyURL("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("failed to parse proxy URL: %v", err)
	}

	err = validateHTTPClientProxyEndpoints(
		[]httpClientProxyEndpoint{{label: "HTTP_PROXY", url: proxyURL}},
		[]listenerAddress{{name: "forward", host: "0.0.0.0", port: 8080}},
	)
	if err == nil {
		t.Fatal("expected self proxy to fail")
	}
	if !strings.Contains(err.Error(), "points to this process") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateHTTPClientProxyRejectsSelfProxyOnResolvedListener(t *testing.T) {
	proxyURL, err := loggingproxy.ParseHTTPClientProxyURL("http://127.0.0.1:5601")
	if err != nil {
		t.Fatalf("failed to parse proxy URL: %v", err)
	}

	err = validateHTTPClientProxyEndpoints(
		[]httpClientProxyEndpoint{{label: "HTTP_PROXY", url: proxyURL}},
		[]listenerAddress{{name: "reverse", host: "localhost", port: 5601}},
	)
	if err == nil {
		t.Fatal("expected self proxy to fail")
	}
	if !strings.Contains(err.Error(), "points to this process") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateHTTPClientProxyAllowsDifferentPort(t *testing.T) {
	proxyURL, err := loggingproxy.ParseHTTPClientProxyURL("http://127.0.0.1:3128")
	if err != nil {
		t.Fatalf("failed to parse proxy URL: %v", err)
	}

	err = validateHTTPClientProxyEndpoints(
		[]httpClientProxyEndpoint{{label: "HTTP_PROXY", url: proxyURL}},
		[]listenerAddress{{name: "forward", host: "127.0.0.1", port: 8080}},
	)
	if err != nil {
		t.Fatalf("expected different port to pass, got %v", err)
	}
}

func TestLoadConfigAllowsProxyOnlyConfig(t *testing.T) {
	config, err := loadConfig(writeTestConfig(t, `
logging:
  enabled: false
proxy:
  host: "127.0.0.1"
  port: 8888
`))
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if config.Server != nil {
		t.Fatalf("expected omitted server to remain nil, got %#v", config.Server)
	}
	if config.Proxy == nil {
		t.Fatal("expected proxy config")
	}
	if config.Proxy.Host != "127.0.0.1" || config.Proxy.Port != 8888 {
		t.Fatalf("unexpected proxy address %s:%d", config.Proxy.Host, config.Proxy.Port)
	}
}

func TestLoadConfigAppliesServerDefaultsWhenServerPresent(t *testing.T) {
	config, err := loadConfig(writeTestConfig(t, `
server: {}
logging:
  enabled: false
`))
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if config.Server == nil {
		t.Fatal("expected server config")
	}
	if config.Server.Host != "localhost" || config.Server.Port != 5601 {
		t.Fatalf("unexpected default server address %s:%d", config.Server.Host, config.Server.Port)
	}
}

func TestLoadConfigRejectsRoutesWithoutServer(t *testing.T) {
	_, err := loadConfig(writeTestConfig(t, `
logging:
  enabled: false
proxy:
  host: "127.0.0.1"
routes:
  api:
    pattern: "/api/"
    destination: "https://example.com/"
`))
	if err == nil {
		t.Fatal("expected routes without server to fail")
	}
	if !strings.Contains(err.Error(), "routes require a server section") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigRejectsConfigWithoutListeners(t *testing.T) {
	_, err := loadConfig(writeTestConfig(t, `
logging:
  enabled: false
`))
	if err == nil {
		t.Fatal("expected config without server or proxy to fail")
	}
	if !strings.Contains(err.Error(), "at least one of server or proxy") {
		t.Fatalf("unexpected error: %v", err)
	}
}
