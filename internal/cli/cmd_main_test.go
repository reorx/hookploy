package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/reorx/hookploy/internal/httpapi"
	"github.com/reorx/hookploy/internal/webui"
)

// Behavior: with the web UI enabled, / redirects to /ui/ and /ui/login serves
// the login page; disabled (ui == nil), both fall through to the API mux and
// 404 — the UI is fully absent, not just unlinked.
func TestMainHandlerWebUIToggle(t *testing.T) {
	get := func(h http.Handler, path string) *http.Response {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result()
	}

	enabled := mainHandler(&httpapi.Server{}, webui.New(nil, nil, nil))
	if resp := get(enabled, "/"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Fatalf("enabled /: got %d %q, want 302 → /ui/", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := get(enabled, "/ui/login"); resp.StatusCode != http.StatusOK {
		t.Fatalf("enabled /ui/login: got %d, want 200", resp.StatusCode)
	}

	disabled := mainHandler(&httpapi.Server{}, nil)
	if resp := get(disabled, "/"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled /: got %d, want 404", resp.StatusCode)
	}
	if resp := get(disabled, "/ui/login"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled /ui/login: got %d, want 404", resp.StatusCode)
	}
}
