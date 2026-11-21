package loggingproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouteAuthorizationClientValues(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("client credential leaked upstream: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	values := []string{"Bearer first", "Bearer second"}
	proxy := NewProxyServer("")
	if err := proxy.AddRouteWithOptions("/api/", backend.URL+"/", &NoOpLogger{}, RouteOptions{
		Authorization: &RouteAuthorization{ClientValues: values},
	}); err != nil {
		t.Fatal(err)
	}
	// Registration must snapshot the list rather than retain the caller's slice.
	values[0] = "Bearer mutated"
	for _, value := range []string{"", "Bearer wrong", "Bearer mutated", "Bearer first", "Bearer second"} {
		req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
		if value != "" {
			req.Header.Set("Authorization", value)
		}
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		want := http.StatusUnauthorized
		if value == "Bearer first" || value == "Bearer second" {
			want = http.StatusNoContent
		}
		if rec.Code != want {
			t.Errorf("credential %q: status = %d, want %d", value, rec.Code, want)
		}
	}
}

func TestRouteAuthorizationRejectsInvalidClientValues(t *testing.T) {
	for _, values := range [][]string{{}, {""}, {"valid", "invalid\r\nheader"}} {
		_, err := validateRouteAuthorization(&RouteAuthorization{
			BackendValue: "Bearer backend",
			ClientValues: values,
		})
		if err == nil {
			t.Errorf("expected invalid client values %q to fail", values)
		}
	}
}
