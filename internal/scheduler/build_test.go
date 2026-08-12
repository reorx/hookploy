package scheduler

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
)

// opNames decodes an execution's ops snapshot back into op names — the same
// round trip an edge does when it receives the task.
func opNames(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var steps []ops.Step
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatalf("ops snapshot does not decode: %v\n%s", err, raw)
	}
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Op
	}
	return strings.Join(names, ",")
}

// Behavior: `on:` is resolved at enqueue time, per instance. Each execution's
// snapshot holds exactly the ops that instance runs, so nothing downstream
// (edge, engine, DB, UI) has to know targeting exists.
func TestBuildDeployOnFiltersPerInstance(t *testing.T) {
	svc := service("app", [][]string{{"main"}, {"sg0", "sg1"}},
		`[{compose.pull: {}}, {run: {argv: [migrate]}, on: [main]}, {compose.up: {}}]`)
	_, execs, err := BuildDeploy(svc, model.KindDeploy, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(execs) != 3 {
		t.Fatalf("want one execution per instance, got %d", len(execs))
	}
	want := map[string]string{
		"main": "compose.pull,run,compose.up",
		"sg0":  "compose.pull,compose.up",
		"sg1":  "compose.pull,compose.up",
	}
	for _, e := range execs {
		if got := opNames(t, e.OpsJSON); got != want[e.Instance] {
			t.Errorf("instance %s ops = %s, want %s", e.Instance, got, want[e.Instance])
		}
	}
}

// Behavior: a pipeline without `on:` produces the very same snapshot for
// every instance — per-instance snapshots are a refinement, not a change.
func TestBuildDeployWithoutOnIsUnchanged(t *testing.T) {
	svc := service("app", [][]string{{"main"}, {"sg0"}},
		`[{compose.pull: {}}, {run: {argv: [migrate]}}, {compose.up: {}}]`)
	_, execs, err := BuildDeploy(svc, model.KindDeploy, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range execs[1:] {
		if !bytes.Equal(e.OpsJSON, execs[0].OpsJSON) {
			t.Fatalf("instances disagree on the snapshot:\n%s\n%s", execs[0].OpsJSON, e.OpsJSON)
		}
	}
	if got := opNames(t, execs[0].OpsJSON); got != "compose.pull,run,compose.up" {
		t.Fatalf("ops = %s", got)
	}
}

// Behavior: a single-instance service may target its one instance; the step
// still runs (the `server:` sugar names that instance after the service).
func TestBuildDeployOnSingleInstance(t *testing.T) {
	svc := service("app", nil, `[{run: {argv: [migrate]}, on: [app]}, {compose.up: {}}]`)
	_, execs, err := BuildDeploy(svc, model.KindDeploy, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := opNames(t, execs[0].OpsJSON); got != "run,compose.up" {
		t.Fatalf("ops = %s", got)
	}
}

// Behavior: tasks carry no targeting (the loader rejects it), so a task's
// snapshot is the whole pipeline, on whichever instance --instance picked.
func TestBuildDeployTaskSnapshot(t *testing.T) {
	svc := service("app", [][]string{{"main"}, {"sg0"}}, `[{compose.up: {}}]`)
	svc.Tasks = map[string][]ops.Step{"probe": mustSteps(t, `[{run: {argv: [probe]}}, {compose.restart: {}}]`)}
	_, execs, err := BuildDeploy(svc, model.KindTask, "probe", "sg0", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(execs) != 1 || execs[0].Instance != "sg0" {
		t.Fatalf("task should target one instance, got %+v", execs)
	}
	if got := opNames(t, execs[0].OpsJSON); got != "run,compose.restart" {
		t.Fatalf("ops = %s", got)
	}
}

// mustSteps parses a flow-style pipeline for tests that build tasks by hand.
func mustSteps(t *testing.T, src string) []ops.Step {
	t.Helper()
	var svcYAML = `
servers:
  s1: { local: true }
services:
  x: { server: s1, dir: /opt/x, deploy: ` + src + ` }
`
	c, err := parseConfigString(svcYAML)
	if err != nil {
		t.Fatal(err)
	}
	return c.Services["x"].Deploy
}
