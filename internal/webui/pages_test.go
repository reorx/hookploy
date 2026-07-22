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
	d, _ := h.mkDeployExec(service, status)
	return d
}

func (h *harness) mkDeployExec(service string, status model.Status) (*model.Deploy, *model.Execution) {
	h.t.Helper()
	d := &model.Deploy{
		ID:        model.NewDeployID(),
		Service:   service,
		Kind:      model.KindDeploy,
		Payload:   json.RawMessage(`{"note":"hi"}`),
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
	return d, ex
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

// Behavior: the service page shows the rollout×instance topology, the deploy
// pipeline with op names and args, tasks, and the deploy history.
func TestServicePage(t *testing.T) {
	h := newHarness(t)
	h.login(h.adminToken)
	d := h.mkDeploy("multi", model.StatusSucceeded)

	resp := h.get("/ui/services/multi")
	if resp.StatusCode != 200 {
		t.Fatalf("service page: %d", resp.StatusCode)
	}
	body := h.body(resp)
	// header facts
	for _, want := range []string{"multi", "ghcr.io/x/multi", "10m0s"} {
		if !strings.Contains(body, want) {
			t.Fatalf("header missing %q", want)
		}
	}
	// rollout topology: both waves, instance cards with server + dir
	for _, want := range []string{"wave 1", "wave 2", "m-a", "m-b", "/opt/m-a", "/opt/m-b", "s2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("topology missing %q", want)
		}
	}
	// pipeline ops in order, args rendered
	for _, want := range []string{"compose.pull", "compose.up", "healthcheck", "http://127.0.0.1:1/health"} {
		if !strings.Contains(body, want) {
			t.Fatalf("pipeline missing %q", want)
		}
	}
	if strings.Index(body, "compose.pull") > strings.Index(body, "healthcheck") {
		t.Fatal("pipeline steps out of order")
	}
	// tasks
	for _, want := range []string{"backup", "backup.sh"} {
		if !strings.Contains(body, want) {
			t.Fatalf("tasks missing %q", want)
		}
	}
	// history row links to the deploy detail
	if !strings.Contains(body, "/ui/deploys/"+d.ID) {
		t.Fatal("history missing deploy link")
	}

	// unknown service → 404
	resp = h.get("/ui/services/ghost")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown service: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Behavior: the deploy detail page shows the header facts, the per-wave
// execution timeline with op records, and mounts the log viewer with the
// execution filter and the exec map for anchor synthesis.
func TestDeployPage(t *testing.T) {
	h := newHarness(t)
	h.login(h.adminToken)
	d, ex := h.mkDeployExec("svc", model.StatusFailed)
	st := h.ui.Store
	st.SetDeployDigest(d.ID, "sha256:abcdef1234567890")
	st.StartOp(ex.ID, 0, "run")
	code := 1
	st.FinishOp(ex.ID, 0, &code, "exit status 1")

	resp := h.get("/ui/deploys/" + d.ID)
	if resp.StatusCode != 200 {
		t.Fatalf("deploy page: %d", resp.StatusCode)
	}
	body := h.body(resp)
	// header: id, status, digest short form, payload details
	for _, want := range []string{d.ID, "failed", "abcdef123456", "&#34;note&#34;", "<details"} {
		if !strings.Contains(body, want) {
			t.Fatalf("header missing %q", want)
		}
	}
	// timeline: wave, execution line, op record with exit code and error
	for _, want := range []string{"wave 1", "svc @ s1", "/opt/svc", "run", "exit 1", "exit status 1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("timeline missing %q", want)
		}
	}
	// log viewer mount: follow target, terminal flag, exec filter, exec map
	for _, want := range []string{
		`data-follow="` + d.ID + `"`, `data-terminal="true"`,
		`id="exec-filter"`, `value="` + ex.ID + `"`, `id="exec-map"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("log viewer missing %q", want)
		}
	}
	// status region is polled while the deploy runs; this one is terminal
	if strings.Contains(body, `data-poll="/ui/fragments/deploys/`) {
		t.Fatal("terminal deploy must not poll the status fragment")
	}

	run, _ := h.mkDeployExec("svc", model.StatusRunning)
	body = h.body(h.get("/ui/deploys/" + run.ID))
	if !strings.Contains(body, `data-poll="/ui/fragments/deploys/`+run.ID+`/status"`) {
		t.Fatal("running deploy should poll the status fragment")
	}

	// unknown deploy → 404
	resp = h.get("/ui/deploys/dp_nope")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown deploy: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Behavior: the status fragment serves header+timeline without the layout;
// anonymous JS fetches get 401.
func TestDeployStatusFragment(t *testing.T) {
	h := newHarness(t)
	d := h.mkDeploy("svc", model.StatusRunning)
	resp := h.get("/ui/fragments/deploys/" + d.ID + "/status")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
	resp.Body.Close()

	h.login(h.adminToken)
	resp = h.get("/ui/fragments/deploys/" + d.ID + "/status")
	if resp.StatusCode != 200 {
		t.Fatalf("fragment: %d", resp.StatusCode)
	}
	body := h.body(resp)
	if strings.Contains(body, "<html") {
		t.Fatal("fragment must not include the layout shell")
	}
	for _, want := range []string{d.ID, "running", "wave 1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("fragment missing %q", want)
		}
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
