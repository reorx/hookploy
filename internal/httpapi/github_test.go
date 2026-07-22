package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

const testWebhookSecret = "test-webhook-secret"

// signGitHub computes X-Hub-Signature-256 independently of the
// implementation under test.
func signGitHub(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *harness) postGitHub(event string, body []byte, sig string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.ts.URL+"/github/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func workflowRunBody(id int64, repo, status, conclusion, createdAt, updatedAt string) []byte {
	concl := "null"
	if conclusion != "" {
		concl = fmt.Sprintf("%q", conclusion)
	}
	return fmt.Appendf(nil, `{
  "action": %q,
  "workflow_run": {
    "id": %d,
    "name": "CI",
    "run_number": 7,
    "status": %q,
    "conclusion": %s,
    "head_branch": "master",
    "head_sha": "0123456789abcdef0123456789abcdef01234567",
    "html_url": "https://github.com/%s/actions/runs/%d",
    "event": "push",
    "display_title": "fix: the thing",
    "created_at": %q,
    "updated_at": %q,
    "run_started_at": %q,
    "actor": {"login": "reorx"}
  },
  "repository": {"full_name": %q}
}`, status, id, status, concl, repo, id, createdAt, updatedAt, createdAt, repo)
}

// Behavior: a correctly signed workflow_run event answers 200 and lands in
// the store with its fields intact.
func TestGitHubWebhookStoresRun(t *testing.T) {
	h := newHarness(t)
	body := workflowRunBody(42, "reorx/linkmind", "in_progress", "",
		"2026-07-22T10:00:00Z", "2026-07-22T10:00:05Z")
	resp := h.postGitHub("workflow_run", body, signGitHub(testWebhookSecret, body))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	runs, err := h.store.ListWorkflowRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	wr := runs[0]
	if wr.ID != 42 || wr.Repo != "reorx/linkmind" || wr.Status != "in_progress" ||
		wr.WorkflowName != "CI" || wr.RunNumber != 7 || wr.HeadBranch != "master" ||
		wr.Actor != "reorx" || wr.Event != "push" || wr.DisplayTitle != "fix: the thing" ||
		wr.HTMLURL != "https://github.com/reorx/linkmind/actions/runs/42" {
		t.Fatalf("fields wrong: %+v", wr)
	}
	if wr.Conclusion != "" {
		t.Fatalf("conclusion should be empty before completion: %q", wr.Conclusion)
	}
	if wr.RunStartedAt == nil {
		t.Fatal("run_started_at lost")
	}
}

// Behavior: a missing or wrong signature answers 401 and nothing is stored.
func TestGitHubWebhookBadSignature(t *testing.T) {
	h := newHarness(t)
	body := workflowRunBody(42, "reorx/linkmind", "queued", "",
		"2026-07-22T10:00:00Z", "2026-07-22T10:00:00Z")
	for _, sig := range []string{"", "sha256=" + strings.Repeat("0", 64), signGitHub("wrong-secret", body)} {
		resp := h.postGitHub("workflow_run", body, sig)
		if resp.StatusCode != 401 {
			t.Fatalf("sig %q: status = %d, want 401", sig, resp.StatusCode)
		}
	}
	runs, err := h.store.ListWorkflowRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("unauthenticated events must not be stored: %+v", runs)
	}
}

// Behavior: without github.webhook_secret in the config the endpoint is
// closed (404), even for a correctly signed request.
func TestGitHubWebhookDisabled(t *testing.T) {
	h := newHarness(t)
	noGithub := strings.Replace(testConfig, "github:\n  webhook_secret: "+testWebhookSecret+"\n", "", 1)
	if noGithub == testConfig {
		t.Fatal("test setup: github section not stripped")
	}
	if err := os.WriteFile(h.cfgPath, []byte(noGithub), 0o644); err != nil {
		t.Fatal(err)
	}
	if resp := h.adminReq("POST", "/-/reload", ""); resp.StatusCode != 200 {
		t.Fatalf("reload failed: %d", resp.StatusCode)
	}
	body := workflowRunBody(42, "reorx/linkmind", "queued", "",
		"2026-07-22T10:00:00Z", "2026-07-22T10:00:00Z")
	resp := h.postGitHub("workflow_run", body, signGitHub(testWebhookSecret, body))
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// Behavior: a signed ping event answers 200; other event types answer 200
// without storing anything (GitHub must not see delivery failures).
func TestGitHubWebhookOtherEvents(t *testing.T) {
	h := newHarness(t)
	ping := []byte(`{"zen": "Keep it logically awesome."}`)
	if resp := h.postGitHub("ping", ping, signGitHub(testWebhookSecret, ping)); resp.StatusCode != 200 {
		t.Fatalf("ping status = %d", resp.StatusCode)
	}
	push := []byte(`{"ref": "refs/heads/master"}`)
	if resp := h.postGitHub("push", push, signGitHub(testWebhookSecret, push)); resp.StatusCode != 200 {
		t.Fatalf("push status = %d", resp.StatusCode)
	}
	runs, err := h.store.ListWorkflowRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("non-workflow_run events must not be stored: %+v", runs)
	}
}

// Behavior: successive deliveries of the same run update one row end to end,
// and a late delivery with an older updated_at does not regress it.
func TestGitHubWebhookUpsertAndOrder(t *testing.T) {
	h := newHarness(t)
	deliver := func(status, conclusion, updatedAt string) {
		body := workflowRunBody(42, "reorx/linkmind", status, conclusion,
			"2026-07-22T10:00:00Z", updatedAt)
		if resp := h.postGitHub("workflow_run", body, signGitHub(testWebhookSecret, body)); resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	deliver("in_progress", "", "2026-07-22T10:00:05Z")
	deliver("completed", "success", "2026-07-22T10:02:00Z")
	deliver("in_progress", "", "2026-07-22T10:01:00Z") // late redelivery

	runs, err := h.store.ListWorkflowRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 row, got %d", len(runs))
	}
	if runs[0].Status != "completed" || runs[0].Conclusion != "success" {
		t.Fatalf("late delivery regressed the row: %+v", runs[0])
	}
}

// Behavior: an oversized body answers 413; signed but malformed or
// incomplete payloads answer 400.
func TestGitHubWebhookBadBody(t *testing.T) {
	h := newHarness(t)
	big := bytes.Repeat([]byte("x"), maxBodyBytes+1)
	if resp := h.postGitHub("workflow_run", big, signGitHub(testWebhookSecret, big)); resp.StatusCode != 413 {
		t.Fatalf("oversized body: status = %d, want 413", resp.StatusCode)
	}
	bad := []byte(`{not json`)
	if resp := h.postGitHub("workflow_run", bad, signGitHub(testWebhookSecret, bad)); resp.StatusCode != 400 {
		t.Fatalf("malformed JSON: status = %d, want 400", resp.StatusCode)
	}
	noID := []byte(`{"workflow_run": {"status": "queued"}, "repository": {"full_name": "a/b"}}`)
	if resp := h.postGitHub("workflow_run", noID, signGitHub(testWebhookSecret, noID)); resp.StatusCode != 400 {
		t.Fatalf("missing run id: status = %d, want 400", resp.StatusCode)
	}
}
