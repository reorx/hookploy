package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/scheduler"
	"github.com/reorx/hookploy/internal/token"
)

// handleWebhook is POST /hooks/{service}: authenticate the service token
// (Bearer or the legacy X-Deploy-Token header), snapshot+interpolate the
// pipeline, and answer 202. Interpolation/digest failures are recorded as a
// failed deploy but still answered 202 — the CI contract has a single path.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")

	plain := bearerToken(r)
	if plain == "" {
		plain = r.Header.Get("X-Deploy-Token")
	}
	if plain == "" {
		writeError(w, http.StatusUnauthorized, "missing token")
		return
	}
	rec, err := s.Store.LookupToken(token.Hash(plain))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rec == nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	svc := s.Config().Services[service]
	if svc == nil {
		writeError(w, http.StatusNotFound, "unknown service "+service)
		return
	}
	if rec.Kind != string(token.KindService) || rec.Subject != service {
		writeError(w, http.StatusForbidden, "token is not valid for service "+service)
		return
	}
	if !svc.Webhook {
		writeError(w, http.StatusForbidden, "service "+service+" does not accept webhooks")
		return
	}

	payload, raw, ok := readPayload(w, r)
	if !ok {
		return
	}
	s.enqueue(w, svc, model.KindDeploy, "", "", payload, raw)
}

// enqueue builds and persists the deploy; build failures become a failed
// deploy record. Both paths answer 202 with the deploy id.
func (s *Server) enqueue(w http.ResponseWriter, svc *config.Service, kind model.Kind, task, instance string, payload map[string]any, raw json.RawMessage) {
	d, execs, err := scheduler.BuildDeploy(svc, kind, task, instance, payload, raw)
	if err != nil {
		now := time.Now()
		d = &model.Deploy{
			ID:         model.NewDeployID(),
			Service:    svc.Name,
			Kind:       kind,
			Task:       task,
			Payload:    raw,
			Status:     model.StatusFailed,
			Error:      err.Error(),
			CreatedAt:  now,
			FinishedAt: &now,
		}
		if err := s.Store.CreateDeploy(d, nil); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, api.Accepted{DeployID: d.ID, StatusURL: "/deploys/" + d.ID})
		return
	}
	if err := s.Store.CreateDeploy(d, execs); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Sched.Enqueue(d.Service, d.ID)
	writeJSON(w, http.StatusAccepted, api.Accepted{DeployID: d.ID, StatusURL: "/deploys/" + d.ID})
}

// handleTriggerDeploy is POST /services/{name}/deploy — the manual/CLI
// equivalent of a webhook; it works even with webhook:false.
func (s *Server) handleTriggerDeploy(w http.ResponseWriter, r *http.Request) {
	svc := s.Config().Services[r.PathValue("name")]
	if svc == nil {
		writeError(w, http.StatusNotFound, "unknown service "+r.PathValue("name"))
		return
	}
	payload, raw, ok := readPayload(w, r)
	if !ok {
		return
	}
	s.enqueue(w, svc, model.KindDeploy, "", "", payload, raw)
}

// handleTriggerTask is POST /services/{name}/tasks/{task}?instance=…
func (s *Server) handleTriggerTask(w http.ResponseWriter, r *http.Request) {
	svc := s.Config().Services[r.PathValue("name")]
	if svc == nil {
		writeError(w, http.StatusNotFound, "unknown service "+r.PathValue("name"))
		return
	}
	taskName := r.PathValue("task")
	if svc.Tasks[taskName] == nil {
		writeError(w, http.StatusNotFound, "service "+svc.Name+" has no task "+taskName)
		return
	}
	payload, raw, ok := readPayload(w, r)
	if !ok {
		return
	}
	s.enqueue(w, svc, model.KindTask, taskName, r.URL.Query().Get("instance"), payload, raw)
}

// handleReload is POST /-/reload.
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if err := s.Reload(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
