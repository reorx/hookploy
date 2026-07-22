package webui

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func (h *harness) body(resp *http.Response) string {
	h.t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return string(b)
}

// Behavior: page routes are session-guarded — anonymous visitors are
// redirected to the login page.
func TestPagesRequireSession(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/ui/", "/ui/services/svc", "/ui/deploys/dp_x"} {
		resp := h.get(path)
		if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
			t.Fatalf("GET %s anonymous: %d, want redirect", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/ui/login" {
			t.Fatalf("GET %s redirects to %q, want /ui/login", path, loc)
		}
		resp.Body.Close()
	}
}

// Behavior: the login page renders a token form; a failed login re-renders
// it with an error; a logged-in visitor is bounced to the dashboard.
func TestLoginPage(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/ui/login")
	if resp.StatusCode != 200 {
		t.Fatalf("login page: %d", resp.StatusCode)
	}
	body := h.body(resp)
	if !strings.Contains(body, `name="token"`) || !strings.Contains(body, `type="password"`) {
		t.Fatalf("login form missing: %s", body)
	}

	resp, err := h.client.PostForm(h.ts.URL+"/ui/login", url.Values{"token": {"hpa_wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("failed login: %d", resp.StatusCode)
	}
	if body := h.body(resp); !strings.Contains(body, `name="token"`) || !strings.Contains(strings.ToLower(body), "invalid") {
		t.Fatalf("failed login should re-render the form with an error: %s", body)
	}

	h.login(h.adminToken)
	resp = h.get("/ui/login")
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("logged-in login page should redirect: %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("redirect to %q, want /ui/", loc)
	}
	resp.Body.Close()
}

// Behavior: a logged-in dashboard request renders the layout shell with the
// static assets wired up.
func TestDashboardShell(t *testing.T) {
	h := newHarness(t)
	h.login(h.adminToken)
	resp := h.get("/ui/")
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard: %d", resp.StatusCode)
	}
	body := h.body(resp)
	for _, want := range []string{"hookploy", "/ui/static/app.css", "/ui/static/app.js"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q: %s", want, body)
		}
	}
	// static assets are served
	for _, path := range []string{"/ui/static/app.css", "/ui/static/app.js", "/ui/static/logs.js"} {
		resp := h.get(path)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
