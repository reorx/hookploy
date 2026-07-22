package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/scheduler"
	"github.com/reorx/hookploy/internal/version"
)

func (s *Server) handleGetDeploy(w http.ResponseWriter, r *http.Request) {
	d, execs, opsByExec, err := s.loadDeploy(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeError(w, http.StatusNotFound, "deploy not found")
		return
	}
	writeJSON(w, http.StatusOK, api.FromDeploy(d, execs, opsByExec))
}

func (s *Server) loadDeploy(id string) (*model.Deploy, []*model.Execution, map[string][]*model.OpRecord, error) {
	d, err := s.Store.GetDeploy(id)
	if err != nil || d == nil {
		return d, nil, nil, err
	}
	execs, err := s.Store.ListExecutions(id)
	if err != nil {
		return nil, nil, nil, err
	}
	opsByExec := map[string][]*model.OpRecord{}
	for _, ex := range execs {
		recs, err := s.Store.ListOpRecords(ex.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		opsByExec[ex.ID] = recs
	}
	return d, execs, opsByExec, nil
}

// handleLogs serves GET /deploys/{id}/logs. Default: raw text replay.
// ?format=json: NDJSON of api.LogLine. ?follow=1: NDJSON stream (replay +
// live) ending with a final {"done":true,...} line when the deploy settles.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("follow") == "1" {
		s.followLogs(w, r, id)
		return
	}
	d, err := s.Store.GetDeploy(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeError(w, http.StatusNotFound, "deploy not found")
		return
	}
	lines, err := s.Store.GetDeployLogs(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := newNDJSON(w)
		for _, l := range lines {
			enc(api.FromLogLine(l))
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, l := range lines {
		io.WriteString(w, l.Data)
	}
}

func (s *Server) followLogs(w http.ResponseWriter, r *http.Request, id string) {
	events, cancel, err := s.Store.FollowDeploy(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := newNDJSON(w)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Line != nil {
				enc(api.FromLogLine(ev.Line))
			}
			if ev.Done {
				enc(api.LogDone{Done: true, Status: string(ev.Status)})
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	latest, err := s.Store.LatestDeploys()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []api.ServiceSummary{}
	for _, name := range cfg.ServiceNames {
		svc := cfg.Services[name]
		row := api.ServiceSummary{Name: name, Webhook: svc.Webhook}
		for _, inst := range svc.Instances {
			row.Servers = append(row.Servers, inst.Server)
		}
		if d := latest[name]; d != nil {
			row.LastDeploy = api.FromDeploy(d, nil, nil)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRecentDeploys serves GET /deploys?limit=N: newest deploys across all
// services, without executions. limit defaults to 20, capped at 100.
func (s *Server) handleRecentDeploys(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}
	deploys, err := s.Store.ListRecentDeploys(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []*api.Deploy{}
	for _, d := range deploys {
		out = append(out, api.FromDeploy(d, nil, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleServiceDetail serves GET /services/{name}: the normalized service
// definition with pipelines in the ops wire format.
func (s *Server) handleServiceDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	svc := s.Config().Services[name]
	if svc == nil {
		writeError(w, http.StatusNotFound, "unknown service "+name)
		return
	}
	out := api.ServiceDetail{
		Name:    svc.Name,
		Image:   svc.Image,
		Webhook: svc.Webhook,
		Timeout: svc.Timeout.String(),
		Rollout: svc.Rollout,
	}
	for _, inst := range svc.Instances {
		out.Instances = append(out.Instances, api.InstanceInfo{Name: inst.Name, Server: inst.Server, Dir: inst.Dir})
	}
	deploySteps, err := marshalSteps(svc.Deploy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.Deploy = deploySteps
	if len(svc.Tasks) > 0 {
		out.Tasks = map[string][]json.RawMessage{}
		for tname, steps := range svc.Tasks {
			enc, err := marshalSteps(steps)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			out.Tasks[tname] = enc
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func marshalSteps(steps []ops.Step) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(steps))
	for _, st := range steps {
		b, err := json.Marshal(st)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func (s *Server) handleServiceDeploys(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.Config().Services[name] == nil {
		writeError(w, http.StatusNotFound, "unknown service "+name)
		return
	}
	deploys, err := s.Store.ListDeploys(name, scheduler.RetainPerService)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []*api.Deploy{}
	for _, d := range deploys {
		out = append(out, api.FromDeploy(d, nil, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleServers reports each server's connectivity: local servers are online
// by definition (they are this process); remote ones are online while their
// edge session is connected.
func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	edges := map[string]model.EdgeInfo{}
	if s.Edges != nil {
		edges = s.Edges()
	}
	out := []api.ServerInfo{}
	for _, name := range sortedServerNames(cfg) {
		srv := cfg.Servers[name]
		row := api.ServerInfo{Name: name, Local: srv.Local, Status: "offline"}
		if srv.Local {
			row.Status = "online"
			row.Version = version.Version
		} else if info, ok := edges[name]; ok {
			row.Status = "online"
			row.Version = info.Version
			at := info.ConnectedAt
			row.ConnectedAt = &at
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}
