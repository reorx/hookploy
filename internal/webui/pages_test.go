package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/model"
)

// mkDeploy inserts a deploy with one execution for the configured service.
func (h *harness) mkDeploy(service string, status model.Status) *model.Deploy {
	h.t.Helper()
	d := &model.Deploy{
		ID:        model.NewDeployID(),
		Service:   service,
		Kind:      model.KindDeploy,
		Payload:   json.RawMessage(`{}`),
		Status:    status,
		CreatedAt: time.Now(),
	}
	ex := &model.Execution{
		ID:        model.NewExecutionID(),
		DeployID:  d.ID,
		Service:   service,
		Instance:  service,
		Server:    "s1",
		Dir:       "/opt/svc",
		OpsJSON:   json.RawMessage(`[{"op":"run","args":{"argv":["x"]}}]`),
		Timeout:   model.Duration(10 * time.Minute),
		Status:    status,
		CreatedAt: time.Now(),
	}
	if err := h.ui.Store.CreateDeploy(d, []*model.Execution{ex}); err != nil {
		h.t.Fatal(err)
	}
	return d
}

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

// Behavior: the dashboard shows the three sections — active deploys (only
// non-terminal), the service list, and recent deploys including superseded.
func TestDashboardSections(t *testing.T) {
	h := newHarness(t)
	h.login(h.adminToken)

	// no deploys yet: empty-state text, service row still present
	body := h.body(h.get("/ui/"))
	if !strings.Contains(body, "当前没有进行中的部署") {
		t.Fatalf("empty active state missing: %s", body)
	}
	if !strings.Contains(body, `/ui/services/svc`) {
		t.Fatalf("service row link missing: %s", body)
	}

	done := h.mkDeploy("svc", model.StatusSucceeded)
	sup := h.mkDeploy("svc", model.StatusSuperseded)
	run := h.mkDeploy("svc", model.StatusRunning)

	body = h.body(h.get("/ui/"))
	if strings.Contains(body, "当前没有进行中的部署") {
		t.Fatal("active section should show the running deploy")
	}
	// active card: deploy id, per-execution line, detail link, log window mount
	for _, want := range []string{run.ID, "svc @ s1", "/ui/deploys/" + run.ID, `data-follow="` + run.ID + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("active card missing %q", want)
		}
	}
	// recent section lists all three, superseded included
	for _, want := range []string{done.ID, sup.ID, "superseded"} {
		if !strings.Contains(body, want) {
			t.Fatalf("recent section missing %q", want)
		}
	}
}

// Behavior: the dashboard fragment serves the sections without the layout
// shell; without a session it answers 401 (no redirect — it's fetched by JS).
func TestDashboardFragment(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/ui/fragments/dashboard")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous fragment: %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	h.login(h.adminToken)
	h.mkDeploy("svc", model.StatusSucceeded)
	resp = h.get("/ui/fragments/dashboard")
	if resp.StatusCode != 200 {
		t.Fatalf("fragment: %d", resp.StatusCode)
	}
	body := h.body(resp)
	if strings.Contains(body, "<html") {
		t.Fatal("fragment must not include the layout shell")
	}
	if !strings.Contains(body, "/ui/services/svc") {
		t.Fatalf("fragment missing service section: %s", body)
	}
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
