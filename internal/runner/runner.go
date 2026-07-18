// Package runner executes argv commands. Nothing ever passes through a
// shell: Cmd.Argv is handed to exec verbatim.
package runner

import (
	"context"
	"io"
)

// Cmd is one command invocation.
type Cmd struct {
	Argv   []string
	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

// Runner runs commands. A non-zero exit is (code, nil); err is reserved for
// failures to start/kill/context cancellation.
type Runner interface {
	Run(ctx context.Context, cmd Cmd) (exitCode int, err error)
}
