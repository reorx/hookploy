package runner

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// ExecRunner runs commands on the local machine. Each command gets its own
// process group; on context cancellation the whole group receives SIGTERM,
// then SIGKILL after a grace period.
type ExecRunner struct {
	// KillGrace is the SIGTERM→SIGKILL delay. Zero means 5s.
	KillGrace time.Duration
}

func (r *ExecRunner) Run(ctx context.Context, c Cmd) (int, error) {
	if len(c.Argv) == 0 {
		return -1, errors.New("empty argv")
	}
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return -1, err
	}

	grace := r.KillGrace
	if grace == 0 {
		grace = 5 * time.Second
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return exitCode(err), waitErr(err)
	case <-ctx.Done():
		pgid := -cmd.Process.Pid // negative pid = whole process group
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(grace):
			_ = syscall.Kill(pgid, syscall.SIGKILL)
			<-done
		}
		return -1, ctx.Err()
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// waitErr maps a plain non-zero exit to nil (the code carries the info).
func waitErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil
	}
	return err
}
