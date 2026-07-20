package cli

import (
	"flag"
	"fmt"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/config"
)

func cmdValidate(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	file := fs.String("f", "hookploy.yaml", "path to hookploy.yaml")
	asJSON := fs.Bool("json", false, "output JSON")
	// parseInterleaved, not fs.Parse: a bare Parse stops at the first
	// positional, so `validate broken.yaml --json` would drop --json and
	// silently validate the default ./hookploy.yaml instead.
	pos, ok := parseInterleaved(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 0 {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy validate [-f hookploy.yaml] [--json]")
		return 2
	}
	cfg, err := config.Load(*file)
	if err != nil {
		// --json reports the failure on stdout as data; the exit code stays 1.
		if *asJSON {
			if err := printJSON(ctx, api.ValidateResult{Error: err.Error()}); err != nil {
				return jsonWriteFail(ctx, err)
			}
			return 1
		}
		fmt.Fprintf(ctx.Stderr, "invalid: %v\n", err)
		return 1
	}
	if *asJSON {
		if err := printJSON(ctx, api.ValidateResult{OK: true, Servers: len(cfg.Servers), Services: len(cfg.Services)}); err != nil {
			return jsonWriteFail(ctx, err)
		}
		return 0
	}
	fmt.Fprintf(ctx.Stdout, "OK: %d servers, %d services\n", len(cfg.Servers), len(cfg.Services))
	return 0
}
