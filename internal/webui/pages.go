package webui

import (
	"net/http"
	"sort"

	"github.com/a-h/templ"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/version"
	"github.com/reorx/hookploy/internal/webui/views"
)

func render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = c.Render(r.Context(), w)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.SessionValid(r) {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	render(w, r, http.StatusOK, views.Login(""))
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, views.Dashboard(s.serverChips()))
}

func (s *Server) handleServicePage(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r) // implemented in M4 step 6
}

func (s *Server) handleDeployPage(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r) // implemented in M4 step 7
}

// serverChips mirrors httpapi's GET /servers logic for the topbar strip.
func (s *Server) serverChips() []views.ServerChip {
	cfg := s.Config()
	edges := map[string]model.EdgeInfo{}
	if s.Edges != nil {
		edges = s.Edges()
	}
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]views.ServerChip, 0, len(names))
	for _, name := range names {
		srv := cfg.Servers[name]
		chip := views.ServerChip{Name: name, Local: srv.Local}
		if srv.Local {
			chip.Online = true
			chip.Version = version.Version
		} else if info, ok := edges[name]; ok {
			chip.Online = true
			chip.Version = info.Version
		}
		out = append(out, chip)
	}
	return out
}
