package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	loggingproxy "github.com/mrexodia/logging-proxy"
)

func TestClientKeysEndToEnd(t *testing.T) {
	for _, customHeader := range []bool{false, true} {
		for _, backendKey := range []string{"", "backend-secret"} {
			t.Run(fmt.Sprintf("custom=%t/backend=%s", customHeader, backendKey), func(t *testing.T) {
				header, template, prefix := "Authorization", "Bearer {}", "Bearer "
				if customHeader {
					header, template, prefix = "X-API-Key", "{}", ""
				}
				backendHeaders := make(chan string, 10)
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					backendHeaders <- r.Header.Get(header)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer backend.Close()

				// Rebuild with the second key removed to simulate revocation on restart.
				for _, revoked := range []bool{false, true} {
					keys := `["first", "second"]`
					if revoked {
						keys = `["first"]`
					}
					config, err := loadConfig(writeTestConfig(t, fmt.Sprintf(`
server: {}
routes:
  api:
    pattern: /api/
    destination: %q
    authorization:
      header: %q
      template: %q
      backend_key: %q
      client_key: legacy
      client_keys: %s
`, backend.URL+"/", header, template, backendKey, keys)))
					if err != nil {
						t.Fatal(err)
					}
					handler, err := buildReverseProxy(config, &loggingproxy.NoOpLogger{}, loggingproxy.HTTPClientProxyConfig{})
					if err != nil {
						t.Fatal(err)
					}
					for _, key := range []string{"", "wrong", "backend-secret", "legacy", "first", "second"} {
						req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
						if key != "" {
							req.Header.Set(header, prefix+key)
						}
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, req)
						allowed := key == "legacy" || key == "first" || (key == "second" && !revoked)
						if !allowed {
							if rec.Code != http.StatusUnauthorized {
								t.Fatalf("key %q (revoked=%t): status = %d, want 401", key, revoked, rec.Code)
							}
							select {
							case <-backendHeaders:
								t.Fatal("unauthorized request reached backend")
							default:
							}
							continue
						}
						if rec.Code != http.StatusNoContent {
							t.Fatalf("key %q: status = %d, want 204", key, rec.Code)
						}
						want := ""
						if backendKey != "" {
							want = prefix + backendKey
						}
						if got := <-backendHeaders; got != want {
							t.Fatalf("upstream header = %q, want %q", got, want)
						}
					}
				}
			})
		}
	}
}

func TestLoadConfigClientKeysEnvironment(t *testing.T) {
	const processVar = "LOGGING_PROXY_TEST_LIST_PROCESS_KEY"
	const dotenvVar = "LOGGING_PROXY_TEST_LIST_DOTENV_KEY"
	t.Setenv(processVar, "process-key")
	// Ensure dotenv resolution is tested even if the caller has this variable set.
	t.Setenv(dotenvVar, "")
	if err := os.Unsetenv(dotenvVar); err != nil {
		t.Fatal(err)
	}
	path := writeTestConfig(t, `
server: {}
routes:
  api:
    pattern: /api/
    destination: https://example.com/
    authorization:
      client_keys:
        - literal-key
        - "${LOGGING_PROXY_TEST_LIST_PROCESS_KEY}"
        - "${LOGGING_PROXY_TEST_LIST_DOTENV_KEY}"
`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".env"), []byte(processVar+"=overridden\n"+dotenvVar+"=dotenv-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	auth := config.Routes["api"].Authorization
	if !reflect.DeepEqual(auth.ClientKeys, []string{"literal-key", "process-key", "dotenv-key"}) {
		t.Fatalf("unexpected resolved client keys: %#v", auth.ClientKeys)
	}
	if auth.Template != "Bearer {}" || auth.ClientKey != "" || auth.BackendKey != "" {
		t.Fatalf("unexpected authorization defaults: %#v", auth)
	}
}

func TestLoadConfigRejectsInvalidClientKeys(t *testing.T) {
	const variable = "LOGGING_PROXY_TEST_INVALID_LIST_KEY"
	t.Setenv(variable, "")
	for _, tc := range []struct{ name, keys, want string }{
		{"empty list", `[]`, "client_keys must not be an empty list"},
		{"empty key", `["valid", ""]`, "client_keys[1] must not be empty"},
		{"null-only list", `[null]`, "client_keys must not be an empty list"},
		{"invalid header", `["secret\r\nInjected: value"]`, "client_keys[0] is not a valid HTTP header value"},
		{"empty environment", `["${LOGGING_PROXY_TEST_INVALID_LIST_KEY}"]`, "client_keys[0]: environment variable " + variable + " is empty"},
		{"not a list", `single-key`, "cannot unmarshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeTestConfig(t, `
server: {}
routes:
  api:
    pattern: /api/
    destination: https://example.com/
    authorization:
      backend_key: backend-secret
      client_keys: `+tc.keys+"\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := os.Unsetenv(variable); err != nil {
		t.Fatal(err)
	}
	auth := &AuthorizationConfig{ClientKeys: []string{"${" + variable + "}"}}
	if err := resolveRouteAuthorization("api", auth, nil); err == nil || !strings.Contains(err.Error(), "client_keys[0]: environment variable "+variable+" is not set") {
		t.Fatalf("expected missing environment variable error, got %v", err)
	}
}
