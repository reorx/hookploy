package notify

import (
	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
)

// recoveryLookback bounds how far back we look for the run before this one
// when deciding whether a success is a recovery. A window, not a guarantee:
// if it is exhausted the event stays deploy.succeeded, which is off by
// default anyway.
const recoveryLookback = 20

// buildEvent turns a settled deploy into the event to send, or reports that
// there is nothing to send. ok is false when the deploy is not a notifiable
// outcome at all, when its service is gone from the config, or when the
// service's policy does not ask for this kind.
func (h *Hub) buildEvent(cfg *config.Config, d *model.Deploy) (Event, bool, error) {
	svc := cfg.Services[d.Service]
	if svc == nil {
		return Event{}, false, nil // service dropped from the config since
	}
	kind, ok := classify(d.Status)
	if !ok {
		return Event{}, false, nil
	}
	if kind == model.EventDeploySucceeded {
		// Reading history only pays off if one of the two success flavors is
		// actually wanted — under the default policy neither is, so the
		// common case never touches the store for this.
		if !svc.Notify.Wants(model.EventDeploySucceeded) && !svc.Notify.Wants(model.EventDeployRecovered) {
			return Event{}, false, nil
		}
		recovered, err := h.recovered(d)
		if err != nil {
			return Event{}, false, err
		}
		if recovered {
			kind = model.EventDeployRecovered
		}
	}
	if !svc.Notify.Wants(kind) {
		return Event{}, false, nil
	}

	execs, err := h.Store.ListExecutions(d.ID)
	if err != nil {
		return Event{}, false, err
	}
	de := &DeployEvent{
		Service:  d.Service,
		Task:     d.Task,
		DeployID: d.ID,
		Status:   d.Status,
		Error:    d.Error,
	}
	if d.FinishedAt != nil {
		de.FinishedAt = *d.FinishedAt
	}
	if cfg.Notify.BaseURL != "" {
		de.URL = cfg.Notify.BaseURL + "/ui/deploys/" + d.ID
	}
	for _, ex := range execs {
		if ex.Status == model.StatusSucceeded {
			continue
		}
		de.Instances = append(de.Instances, InstanceResult{
			Instance: ex.Instance, Server: ex.Server, Status: ex.Status, Error: ex.Error,
		})
	}
	return Event{Kind: kind, CreatedAt: d.CreatedAt, Deploy: de}, true, nil
}

// classify maps a settled deploy's status onto the event vocabulary. A
// success maps to deploy.succeeded here; whether it is really a recovery
// needs the service's history and is decided by the caller.
//
// A wholly canceled deploy is not an event: that only happens when main is
// shutting down and cancels the waves it never dispatched, which is nobody's
// incident. A failure that cancels its later waves aggregates to failed, not
// canceled, so it still reports. Superseded never ran at all.
func classify(s model.Status) (model.EventKind, bool) {
	switch s {
	case model.StatusFailed:
		return model.EventDeployFailed, true
	case model.StatusUnreachable:
		return model.EventDeployUnreachable, true
	case model.StatusSucceeded:
		return model.EventDeploySucceeded, true
	default:
		return "", false
	}
}

// recovered reports whether the run before d left this service broken.
// Deploys that never ran (superseded) and ones still moving say nothing
// about the service's health, so the search walks past them.
func (h *Hub) recovered(d *model.Deploy) (bool, error) {
	recent, err := h.Store.ListDeploys(d.Service, recoveryLookback)
	if err != nil {
		return false, err
	}
	found := false
	for _, prev := range recent { // newest first
		if !found {
			found = prev.ID == d.ID
			continue
		}
		switch prev.Status {
		case model.StatusFailed, model.StatusUnreachable, model.StatusCanceled:
			return true, nil
		case model.StatusSucceeded:
			return false, nil
		}
	}
	return false, nil
}
