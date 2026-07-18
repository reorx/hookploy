package cli

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/httpapi"
	"github.com/reorx/hookploy/internal/runner"
	"github.com/reorx/hookploy/internal/scheduler"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

const remoteConfig = `
servers:
  s1: { local: true }
services:
  linkmind:
    server: s1
    dir: /opt/apps/linkmind
    deploy:
      - run: { argv: [deploy-linkmind] }
    tasks:
      hello:
        - run: { argv: [say-hello] }
`

// startMain assembles an in-process main and points the CLI env at it.
func startMain(t *testing.T) (*store.Store, *runner.FakeRunner) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hookploy.yaml")
	if err := os.WriteFile(cfgPath, []byte(remoteConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := &runner.FakeRunner{}
	reg := executor.NewRegistry(50 * time.Millisecond)
	reg.Register("s1", &executor.Local{Engine: &engine.Engine{
		Runner: fake,
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}})
	sched := scheduler.New(st, reg)
	t.Cleanup(sched.Shutdown)
	srv := &httpapi.Server{
		Store:  st,
		Sched:  sched,
		Config: func() *config.Config { return cfg },
		Reload: func() error { return nil },
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	adminTok := token.New(token.KindAdmin)
	st.InsertToken(string(token.KindAdmin), "admin", token.Hash(adminTok))
	t.Setenv("HOOKPLOY_URL", ts.URL)
	t.Setenv("HOOKPLOY_ADMIN_TOKEN", adminTok)
	return st, fake
}

// Behavior: without the env vars, remote commands fail with guidance.
func TestRemoteCommandsNeedEnv(t *testing.T) {
	t.Setenv("HOOKPLOY_URL", "")
	t.Setenv("HOOKPLOY_ADMIN_TOKEN", "")
	code, _, errOut := runCLI(t, "status")
	if code != 1 || !strings.Contains(errOut, "HOOKPLOY_URL") {
		t.Fatalf("exit=%d stderr=%q", code, errOut)
	}
}

// Behavior: `deploy` triggers and prints the deploy id; `status --json` and
// `deploys --json` emit exactly the api DTOs.
func TestDeployStatusDeploysJSON(t *testing.T) {
	startMain(t)

	code, out, errOut := runCLI(t, "deploy", "linkmind")
	if code != 0 {
		t.Fatalf("deploy: %d %s", code, errOut)
	}
	if !strings.Contains(out, "dp_") {
		t.Fatalf("deploy output should contain the deploy id: %q", out)
	}

	waitCLI(t, func() bool {
		_, out, _ := runCLI(t, "deploys", "linkmind", "--json")
		var list []*api.Deploy
		if json.Unmarshal([]byte(out), &list) != nil {
			return false
		}
		return len(list) == 1 && list[0].Status == "succeeded"
	})

	code, out, _ = runCLI(t, "status", "--json")
	if code != 0 {
		t.Fatal("status --json failed")
	}
	var st api.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json must decode as api.Status: %v\n%s", err, out)
	}
	if len(st.Servers) != 1 || st.Servers[0].Status != "online" {
		t.Fatalf("servers: %+v", st.Servers)
	}
	if len(st.Services) != 1 || st.Services[0].LastDeploy == nil {
		t.Fatalf("services: %+v", st.Services)
	}

	// human-readable variants also work
	code, out, _ = runCLI(t, "status")
	if code != 0 || !strings.Contains(out, "linkmind") || !strings.Contains(out, "online") {
		t.Fatalf("status text: %q", out)
	}
	code, out, _ = runCLI(t, "deploys", "linkmind")
	if code != 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("deploys text: %q", out)
	}
}

// Behavior: `logs` prints the deploy output; `logs -f` follows to the end
// and exits non-zero for failed deploys.
func TestLogsCommand(t *testing.T) {
	_, fake := startMain(t)
	fake.On("deploy-linkmind").Returning("release deployed!\n", 0).Once = true

	code, out, errOut := runCLI(t, "deploy", "linkmind", "--json")
	if code != 0 {
		t.Fatalf("deploy: %s", errOut)
	}
	var acc api.Accepted
	if err := json.Unmarshal([]byte(out), &acc); err != nil {
		t.Fatal(err)
	}

	// follow until terminal: prints logs, exits 0 on success
	code, out, errOut = runCLI(t, "logs", acc.DeployID, "-f")
	if code != 0 {
		t.Fatalf("logs -f exit %d, stderr %s", code, errOut)
	}
	if !strings.Contains(out, "release deployed!") {
		t.Fatalf("logs -f output: %q", out)
	}

	// plain logs after the fact
	code, out, _ = runCLI(t, "logs", acc.DeployID)
	if code != 0 || !strings.Contains(out, "release deployed!") {
		t.Fatalf("logs output: %q", out)
	}

	// failed deploy → logs -f exits 1
	fake.On("deploy-linkmind").Returning("boom\n", 3)
	_, out, _ = runCLI(t, "deploy", "linkmind", "--json")
	json.Unmarshal([]byte(out), &acc)
	code, _, _ = runCLI(t, "logs", acc.DeployID, "-f")
	if code == 0 {
		t.Fatal("logs -f must exit non-zero when the deploy failed")
	}
}

// Behavior: `task` runs a named task remotely.
func TestTaskCommand(t *testing.T) {
	_, fake := startMain(t)
	code, out, errOut := runCLI(t, "task", "linkmind", "hello")
	if code != 0 {
		t.Fatalf("task: %d %s", code, errOut)
	}
	if !strings.Contains(out, "dp_") {
		t.Fatalf("task output: %q", out)
	}
	waitCLI(t, func() bool {
		for _, c := range fake.JoinedCalls() {
			if c == "say-hello" {
				return true
			}
		}
		return false
	})

	// unknown task fails loudly
	code, _, errOut = runCLI(t, "task", "linkmind", "nope")
	if code == 0 || !strings.Contains(errOut, "nope") {
		t.Fatalf("unknown task: %d %q", code, errOut)
	}
}

func waitCLI(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
