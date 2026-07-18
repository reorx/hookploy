package cli

import (
	"flag"
	"fmt"

	"github.com/reorx/hookploy/internal/config"
)

func cmdValidate(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	file := fs.String("f", "hookploy.yaml", "path to hookploy.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*file)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "invalid: %v\n", err)
		return 1
	}
	fmt.Fprintf(ctx.Stdout, "OK: %d servers, %d services\n", len(cfg.Servers), len(cfg.Services))
	return 0
}
