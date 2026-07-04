package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return path
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
