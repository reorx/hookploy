package webui

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/httpapi"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

const testConfig = `
servers:
  s1: { local: true }
  s2: {} # edge server with no session in tests → offline
services:
  svc:
    server: s1
    dir: /opt/svc
    deploy:
      - run: { argv: [x] }
  multi:
    image: ghcr.io/x/multi
    instances:
      m-a: { server: s1, dir: /opt/m-a }
      m-b: { server: s2, dir: /opt/m-b }
    rollout:
      - m-a
      - m-b
    deploy:
      - compose.pull
      - compose.up
      - healthcheck: { url: "http://127.0.0.1:1/health" }
    tasks:
      backup:
        - run: { argv: [backup.sh] }
`

type harness struct {
	t          *testing.T
	ts         *httptest.Server
	ui         *Server
	client     *http.Client // cookie jar, no redirect following
	adminToken string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hookploy.yaml")
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfgFn := func() *config.Config { return cfg }
	ui := New(st, cfgFn, nil)
	apiSrv := &httpapi.Server{Store: st, Config: cfgFn, SessionOK: ui.SessionValid}
	mux := http.NewServeMux()
	mux.Handle("/ui/", ui.Handler())
	mux.Handle("/", apiSrv.Handler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	adminToken := token.New(token.KindAdmin)
	st.InsertToken(string(token.KindAdmin), "admin", token.Hash(adminToken))

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &harness{t: t, ts: ts, ui: ui, client: client, adminToken: adminToken}
}

func (h *harness) login(tok string) *http.Response {
	h.t.Helper()
	resp, err := h.client.PostForm(h.ts.URL+"/ui/login", url.Values{"token": {tok}})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	resp, err := h.client.Get(h.ts.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// Behavior: a valid admin token logs in — the response redirects to /ui/ and
// sets an HttpOnly session cookie that then authenticates GET admin API calls.
func TestLoginSuccess(t *testing.T) {
	h := newHarness(t)
	resp := h.login(h.adminToken)
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("redirect to %q, want /ui/", loc)
	}
	var sc *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "hookploy_session" {
			sc = c
		}
	}
	if sc == nil {
		t.Fatal("no hookploy_session cookie set")
	}
	if !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || sc.Path != "/" {
		t.Fatalf("cookie attributes: HttpOnly=%v SameSite=%v Path=%q", sc.HttpOnly, sc.SameSite, sc.Path)
	}

	// cookie authenticates a GET admin endpoint without a Bearer header
	resp = h.get("/services")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /services with session cookie: %d", resp.StatusCode)
	}
}

// Behavior: wrong / non-admin tokens are rejected with 401 and no cookie.
func TestLoginFailure(t *testing.T) {
	h := newHarness(t)
	for _, bad := range []string{"", "hpa_wrong", token.New(token.KindService)} {
		resp := h.login(bad)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("login %q: status %d, want 401", bad, resp.StatusCode)
		}
		for _, c := range resp.Cookies() {
			if c.Name == "hookploy_session" && c.Value != "" && c.MaxAge >= 0 {
				t.Fatal("failed login must not set a session cookie")
			}
		}
		resp.Body.Close()
	}
	// a service token stored in the DB must not log in either
	svcTok := token.New(token.KindService)
	h.ui.Store.InsertToken(string(token.KindService), "svc", token.Hash(svcTok))
	resp := h.login(svcTok)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("service token login: %d, want 401", resp.StatusCode)
	}
}

// Behavior: the session cookie only authenticates GET/HEAD — POST endpoints
// still require a Bearer token (CSRF surface stays closed).
func TestCookieRejectedOnPost(t *testing.T) {
	h := newHarness(t)
	h.login(h.adminToken)
	resp, err := h.client.Post(h.ts.URL+"/services/svc/deploy", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST with cookie only: %d, want 401", resp.StatusCode)
	}
}

// Behavior: logout invalidates the session server-side and clears the cookie.
func TestLogout(t *testing.T) {
	h := newHarness(t)
	h.login(h.adminToken)
	if resp := h.get("/services"); resp.StatusCode != 200 {
		t.Fatalf("pre-logout GET: %d", resp.StatusCode)
	}
	resp, err := h.client.Post(h.ts.URL+"/ui/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("logout status %d", resp.StatusCode)
	}
	if resp := h.get("/services"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session must be dead after logout: %d", resp.StatusCode)
	}
}

// Behavior: expired sessions stop authenticating.
func TestSessionExpiry(t *testing.T) {
	h := newHarness(t)
	h.login(h.adminToken)
	h.ui.sessions.expireAll(time.Now().Add(-time.Second))
	if resp := h.get("/services"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired session must not authenticate: %d", resp.StatusCode)
	}
}
