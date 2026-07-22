// Package webui serves the built-in read-only web interface on /ui/. Pages
// are rendered server-side (templ) against the same store/config the HTTP
// API uses — no HTTP self-calls. Authentication is a login form exchanging
// the admin token for an in-memory session cookie; the httpapi admin
// middleware additionally accepts that cookie on GET/HEAD so the UI's JS can
// fetch the existing JSON endpoints same-origin.
package webui

import (
	"net/http"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/store"
)

// Server holds the web UI's dependencies (same injection set as httpapi.Server).
type Server struct {
	Store    *store.Store
	Config   func() *config.Config
	Edges    func() map[string]model.EdgeInfo
	sessions *sessionStore
}

// New builds a Server with a fresh in-memory session store.
func New(st *store.Store, cfg func() *config.Config, edges func() map[string]model.EdgeInfo) *Server {
	return &Server{Store: st, Config: cfg, Edges: edges, sessions: newSessionStore()}
}

// Handler builds the /ui/ routing table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/login", s.handleLogin)
	mux.HandleFunc("POST /ui/logout", s.handleLogout)
	return mux
}
