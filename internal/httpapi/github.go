package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/reorx/hookploy/internal/model"
)

const retainWorkflowRunsPerRepo = 200

// ghWorkflowRunEvent is the subset of the workflow_run webhook payload we
// keep; everything else GitHub sends is ignored.
type ghWorkflowRunEvent struct {
	WorkflowRun struct {
		ID           int64      `json:"id"`
		Name         string     `json:"name"`
		RunNumber    int        `json:"run_number"`
		Status       string     `json:"status"`
		Conclusion   string     `json:"conclusion"`
		HeadBranch   string     `json:"head_branch"`
		HeadSHA      string     `json:"head_sha"`
		HTMLURL      string     `json:"html_url"`
		Event        string     `json:"event"`
		DisplayTitle string     `json:"display_title"`
		CreatedAt    time.Time  `json:"created_at"`
		UpdatedAt    time.Time  `json:"updated_at"`
		RunStartedAt *time.Time `json:"run_started_at"`
		Actor        struct {
			Login string `json:"login"`
		} `json:"actor"`
	} `json:"workflow_run"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// handleGitHubWebhook is POST /github/webhook: verify the HMAC signature,
// upsert workflow_run events, and answer 200 to everything else GitHub
// sends so deliveries never show as failed. The endpoint stays closed (404)
// until github.webhook_secret is configured — without a signature check any
// caller could inject fake build data.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := s.Config().Github.WebhookSecret
	if secret == "" {
		writeError(w, http.StatusNotFound, "github webhook not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "body exceeds 1MiB")
		} else {
			writeError(w, http.StatusBadRequest, "cannot read body: "+err.Error())
		}
		return
	}
	if !verifyGitHubSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	switch r.Header.Get("X-GitHub-Event") {
	case "workflow_run":
	case "ping":
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ignored": true})
		return
	}

	var ev ghWorkflowRunEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: "+err.Error())
		return
	}
	if ev.WorkflowRun.ID == 0 || ev.Repository.FullName == "" {
		writeError(w, http.StatusBadRequest, "payload is not a workflow_run event")
		return
	}

	run := &ev.WorkflowRun
	wr := &model.WorkflowRun{
		ID:           run.ID,
		Repo:         ev.Repository.FullName,
		WorkflowName: run.Name,
		RunNumber:    run.RunNumber,
		Status:       run.Status,
		Conclusion:   run.Conclusion,
		HeadBranch:   run.HeadBranch,
		HeadSHA:      run.HeadSHA,
		HTMLURL:      run.HTMLURL,
		Event:        run.Event,
		Actor:        run.Actor.Login,
		DisplayTitle: run.DisplayTitle,
		CreatedAt:    run.CreatedAt,
		UpdatedAt:    run.UpdatedAt,
		RunStartedAt: run.RunStartedAt,
		ReceivedAt:   time.Now(),
	}
	if err := s.Store.UpsertWorkflowRun(wr); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.CleanupWorkflowRuns(wr.Repo, retainWorkflowRunsPerRepo)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// verifyGitHubSignature checks a GitHub X-Hub-Signature-256 header value
// (constant-time) against the raw request body.
func verifyGitHubSignature(secret string, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	sig, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}
