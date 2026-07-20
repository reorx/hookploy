package cli

import (
	"flag"
	"fmt"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/version"
)

func cmdVersion(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	asJSON := fs.Bool("json", false, "output JSON")
	pos, ok := parseInterleaved(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 0 {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy version [--json]")
		return 2
	}
	if *asJSON {
		if err := printJSON(ctx, api.VersionInfo{Version: version.Version}); err != nil {
			return jsonWriteFail(ctx, err)
		}
		return 0
	}
	fmt.Fprintf(ctx.Stdout, "hookploy %s\n", version.Version)
	return 0
}
