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
	data, err := s.dashboardData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, views.Dashboard(s.serverChips(), data))
}

func (s *Server) handleDashboardFragment(w http.ResponseWriter, r *http.Request) {
	data, err := s.dashboardData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, views.DashboardContent(data))
}

func (s *Server) dashboardData() (views.DashboardData, error) {
	var data views.DashboardData
	cfg := s.Config()
	latest, err := s.Store.LatestDeploys()
	if err != nil {
		return data, err
	}
	for _, name := range cfg.ServiceNames {
		svc := cfg.Services[name]
		row := views.ServiceRow{Name: name, Webhook: svc.Webhook}
		for _, inst := range svc.Instances {
			row.Servers = append(row.Servers, inst.Server)
		}
		if d := latest[name]; d != nil {
			row.LastID, row.LastStatus, row.LastAt = d.ID, string(d.Status), d.CreatedAt
			if !d.Status.Terminal() {
				card, err := s.activeCard(d)
				if err != nil {
					return data, err
				}
				data.Active = append(data.Active, card)
			}
		}
		data.Services = append(data.Services, row)
	}
	recent, err := s.Store.ListRecentDeploys(20)
	if err != nil {
		return data, err
	}
	for _, d := range recent {
		data.Recent = append(data.Recent, deployRow(d))
	}
	return data, nil
}

// activeCard assembles the in-progress card: per-execution lines plus the
// current/total wave counters.
func (s *Server) activeCard(d *model.Deploy) (views.ActiveDeploy, error) {
	card := views.ActiveDeploy{
		ID:        d.ID,
		Service:   d.Service,
		Kind:      views.KindLabel(string(d.Kind), d.Task),
		Status:    string(d.Status),
		CreatedAt: d.CreatedAt,
	}
	execs, err := s.Store.ListExecutions(d.ID)
	if err != nil {
		return card, err
	}
	current := -1
	for _, ex := range execs {
		if ex.Wave+1 > card.Waves {
			card.Waves = ex.Wave + 1
		}
		if !ex.Status.Terminal() && (current == -1 || ex.Wave < current) {
			current = ex.Wave
		}
		card.Execs = append(card.Execs, views.ExecLine{
			Instance: ex.Instance, Server: ex.Server, Status: string(ex.Status), Wave: ex.Wave,
		})
	}
	if current == -1 {
		current = card.Waves - 1
	}
	card.Wave = current + 1
	return card, nil
}

func deployRow(d *model.Deploy) views.DeployRow {
	row := views.DeployRow{
		ID:        d.ID,
		Service:   d.Service,
		Kind:      views.KindLabel(string(d.Kind), d.Task),
		Status:    string(d.Status),
		Digest:    views.ShortDigest(d.Digest),
		CreatedAt: d.CreatedAt,
		Duration:  "—",
	}
	switch d.Status {
	case model.StatusSucceeded, model.StatusFailed, model.StatusUnreachable, model.StatusCanceled:
		row.Duration = views.Elapsed(d.CreatedAt, d.FinishedAt)
	case model.StatusRunning, model.StatusDispatching:
		row.Duration = views.Elapsed(d.CreatedAt, nil)
	}
	return row
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
