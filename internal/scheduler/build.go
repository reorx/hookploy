package scheduler

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
)

// DigestRe is the format of payload.digest, the only reserved payload field.
var DigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// BuildDeploy turns a trigger into a Deploy plus its per-instance
// Executions. Interpolation happens here, at enqueue time: the resulting
// ops snapshot is immutable and self-contained (the M2 edge message).
//
// For tasks, `instance` selects the target; it is required when the service
// has more than one instance.
func BuildDeploy(svc *config.Service, kind model.Kind, task, instance string, payload map[string]any, raw json.RawMessage) (*model.Deploy, []*model.Execution, error) {
	steps := svc.Deploy
	if kind == model.KindTask {
		steps = svc.Tasks[task]
		if steps == nil {
			return nil, nil, fmt.Errorf("service %q has no task %q", svc.Name, task)
		}
	}
	interpolated, err := ops.Interpolate(steps, payload)
	if err != nil {
		return nil, nil, err
	}
	opsJSON, err := json.Marshal(interpolated)
	if err != nil {
		return nil, nil, err
	}

	digest := ""
	if v, ok := payload["digest"]; ok {
		s, isStr := v.(string)
		if !isStr || !DigestRe.MatchString(s) {
			return nil, nil, fmt.Errorf("payload.digest %v is not of form sha256:<64 hex>", v)
		}
		digest = s
	}

	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	d := &model.Deploy{
		ID:        model.NewDeployID(),
		Service:   svc.Name,
		Kind:      kind,
		Task:      task,
		Payload:   raw,
		Digest:    digest,
		Status:    model.StatusQueued,
		CreatedAt: time.Now(),
	}

	newExec := func(inst *config.Instance, wave int) *model.Execution {
		return &model.Execution{
			ID:        model.NewExecutionID(),
			DeployID:  d.ID,
			Service:   svc.Name,
			Instance:  inst.Name,
			Server:    inst.Server,
			Dir:       inst.Dir,
			Image:     svc.Image,
			Wave:      wave,
			OpsJSON:   opsJSON,
			Timeout:   model.Duration(svc.Timeout),
			Status:    model.StatusQueued,
			CreatedAt: time.Now(),
		}
	}

	var execs []*model.Execution
	if kind == model.KindTask {
		if instance == "" {
			if len(svc.Instances) > 1 {
				return nil, nil, fmt.Errorf("service %q has %d instances; --instance is required", svc.Name, len(svc.Instances))
			}
			instance = svc.Instances[0].Name
		}
		inst := svc.Instance(instance)
		if inst == nil {
			return nil, nil, fmt.Errorf("service %q has no instance %q", svc.Name, instance)
		}
		execs = append(execs, newExec(inst, 0))
	} else {
		for waveIdx, wave := range svc.Rollout {
			for _, name := range wave {
				execs = append(execs, newExec(svc.Instance(name), waveIdx))
			}
		}
	}
	return d, execs, nil
}
