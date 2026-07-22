// Package httpapi serves the webhook endpoint and the status API on one
// listener. All responses serialize internal/api DTOs so API output and CLI
// --json output are identical.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/scheduler"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"

	"github.com/reorx/hookploy/internal/api"
)

const maxBodyBytes = 1 << 20 // 1MiB

// Server wires the HTTP layer to store, scheduler and live config.
type Server struct {
	Store  *store.Store
	Sched  *scheduler.Scheduler
	Config func() *config.Config
	Reload func() error
	// Edges reports currently connected edge sessions (nil in M1 setups).
	Edges func() map[string]model.EdgeInfo
}

// Handler builds the routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/{service}", s.handleWebhook)
	mux.HandleFunc("GET /deploys", s.admin(s.handleRecentDeploys))
	mux.HandleFunc("GET /deploys/{id}", s.admin(s.handleGetDeploy))
	mux.HandleFunc("GET /deploys/{id}/logs", s.admin(s.handleLogs))
	mux.HandleFunc("GET /services", s.admin(s.handleServices))
	mux.HandleFunc("GET /services/{name}", s.admin(s.handleServiceDetail))
	mux.HandleFunc("GET /services/{name}/deploys", s.admin(s.handleServiceDeploys))
	mux.HandleFunc("GET /servers", s.admin(s.handleServers))
	mux.HandleFunc("POST /services/{name}/deploy", s.admin(s.handleTriggerDeploy))
	mux.HandleFunc("POST /services/{name}/tasks/{task}", s.admin(s.handleTriggerTask))
	mux.HandleFunc("POST /-/reload", s.admin(s.handleReload))
	return mux
}

// admin guards a handler with admin-token auth.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plain := bearerToken(r)
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
		if rec.Kind != string(token.KindAdmin) {
			writeError(w, http.StatusForbidden, "admin token required")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.Error{Error: msg})
}

// readPayload reads a ≤1MiB JSON-object body. An empty body is an empty
// payload. Returns (payload, raw, ok); on !ok the response is written.
func readPayload(w http.ResponseWriter, r *http.Request) (map[string]any, json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "body exceeds 1MiB")
		} else {
			writeError(w, http.StatusBadRequest, "cannot read body: "+err.Error())
		}
		return nil, nil, false
	}
	if len(body) == 0 {
		return map[string]any{}, json.RawMessage("{}"), true
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: "+err.Error())
		return nil, nil, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		writeError(w, http.StatusBadRequest, "payload must be a JSON object")
		return nil, nil, false
	}
	return obj, json.RawMessage(body), true
}
