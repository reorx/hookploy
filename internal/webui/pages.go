package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/a-h/templ"
	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/scheduler"
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
		ExecMap:   map[string]views.ExecMapEntry{},
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
		card.ExecMap[ex.ID] = execMapEntry(ex)
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
	name := r.PathValue("name")
	svc := s.Config().Services[name]
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	page, err := s.servicePage(svc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, views.ServiceDetail(s.serverChips(), page))
}

func (s *Server) servicePage(svc *config.Service) (views.ServicePage, error) {
	page := views.ServicePage{
		Name:    svc.Name,
		Image:   svc.Image,
		Webhook: svc.Webhook,
		Timeout: svc.Timeout.String(),
	}
	online := map[string]bool{}
	for _, chip := range s.serverChips() {
		online[chip.Name] = chip.Online
	}
	for _, wave := range svc.Rollout {
		cards := make([]views.InstanceCard, 0, len(wave))
		for _, iname := range wave {
			inst := svc.Instance(iname)
			cards = append(cards, views.InstanceCard{
				Name: inst.Name, Server: inst.Server, Dir: inst.Dir, Online: online[inst.Server],
			})
		}
		page.Waves = append(page.Waves, cards)
	}
	var err error
	if page.Deploy, err = stepViews(svc.Deploy); err != nil {
		return page, err
	}
	taskNames := make([]string, 0, len(svc.Tasks))
	for tname := range svc.Tasks {
		taskNames = append(taskNames, tname)
	}
	sort.Strings(taskNames)
	for _, tname := range taskNames {
		steps, err := stepViews(svc.Tasks[tname])
		if err != nil {
			return page, err
		}
		page.Tasks = append(page.Tasks, views.TaskView{Name: tname, Steps: steps})
	}
	history, err := s.Store.ListDeploys(svc.Name, scheduler.RetainPerService)
	if err != nil {
		return page, err
	}
	for _, d := range history {
		page.History = append(page.History, deployRow(d))
	}
	return page, nil
}

// stepViews flattens each op's typed args into sorted k=v pairs for display.
func stepViews(steps []ops.Step) ([]views.StepView, error) {
	out := make([]views.StepView, 0, len(steps))
	for _, st := range steps {
		sv := views.StepView{Op: st.Op}
		if st.Args != nil {
			b, err := json.Marshal(st.Args)
			if err != nil {
				return nil, err
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				return nil, err
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				sv.Args = append(sv.Args, views.KV{K: k, V: argValue(m[k])})
			}
		}
		out = append(out, sv)
	}
	return out, nil
}

func argValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func (s *Server) handleDeployPage(w http.ResponseWriter, r *http.Request) {
	page, found, err := s.deployPage(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	render(w, r, http.StatusOK, views.DeployDetail(s.serverChips(), page))
}

func (s *Server) handleDeployStatusFragment(w http.ResponseWriter, r *http.Request) {
	page, found, err := s.deployPage(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	render(w, r, http.StatusOK, views.DeployStatus(page))
}

func (s *Server) deployPage(id string) (views.DeployPage, bool, error) {
	var page views.DeployPage
	d, err := s.Store.GetDeploy(id)
	if err != nil || d == nil {
		return page, false, err
	}
	row := deployRow(d)
	page = views.DeployPage{
		ID:        d.ID,
		Service:   d.Service,
		Kind:      row.Kind,
		Status:    row.Status,
		Error:     d.Error,
		Digest:    row.Digest,
		CreatedAt: d.CreatedAt,
		Duration:  row.Duration,
		Payload:   prettyPayload(d.Payload),
		Terminal:  d.Status.Terminal(),
		ExecMap:   map[string]views.ExecMapEntry{},
	}
	execs, err := s.Store.ListExecutions(id)
	if err != nil {
		return page, true, err
	}
	for _, ex := range execs {
		ev := views.ExecView{
			ID:       ex.ID,
			Instance: ex.Instance,
			Server:   ex.Server,
			Dir:      ex.Dir,
			Status:   string(ex.Status),
			Error:    ex.Error,
		}
		if ex.StartedAt != nil {
			if ex.FinishedAt != nil || !ex.Status.Terminal() {
				ev.Duration = views.Elapsed(*ex.StartedAt, ex.FinishedAt)
			}
		}
		recs, err := s.Store.ListOpRecords(ex.ID)
		if err != nil {
			return page, true, err
		}
		for _, rec := range recs {
			op := views.OpView{Index: rec.OpIndex, Name: rec.OpName, Error: rec.Error}
			if rec.FinishedAt != nil {
				op.Duration = views.Elapsed(rec.StartedAt, rec.FinishedAt)
			}
			if rec.ExitCode != nil {
				op.ExitCode = fmt.Sprint(*rec.ExitCode)
			}
			ev.Ops = append(ev.Ops, op)
		}
		if len(page.Waves) == 0 || page.Waves[len(page.Waves)-1].Index != ex.Wave+1 {
			page.Waves = append(page.Waves, views.WaveView{Index: ex.Wave + 1})
		}
		last := len(page.Waves) - 1
		page.Waves[last].Execs = append(page.Waves[last].Execs, ev)
		page.Execs = append(page.Execs, views.ExecOption{ID: ex.ID, Label: ex.Instance + " @ " + ex.Server})
		page.ExecMap[ex.ID] = execMapEntry(ex)
	}
	return page, true, nil
}

// execMapEntry maps op indexes to names from the execution's ops snapshot,
// so logs.js can label frames of ops that never got an op record.
func execMapEntry(ex *model.Execution) views.ExecMapEntry {
	entry := views.ExecMapEntry{Instance: ex.Instance, Ops: map[string]string{}}
	var steps []struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(ex.OpsJSON, &steps); err == nil {
		for i, st := range steps {
			entry.Ops[fmt.Sprint(i)] = st.Op
		}
	}
	return entry
}

func prettyPayload(raw json.RawMessage) string {
	trimmed := string(raw)
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return trimmed
	}
	return buf.String()
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
