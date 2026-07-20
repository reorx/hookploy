package cli

import (
	"flag"
	"fmt"

	"github.com/reorx/hookploy/internal/config"
)

// cmdSchema prints the JSON Schema of hookploy.yaml. The output is JSON by
// nature, so unlike the other commands it has no --json flag. Typical use:
//
//	hookploy schema > .hookploy-schema.json
func cmdSchema(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	pos, ok := parseInterleaved(fs, args)
	if !ok {
		return 2
	}
	// The schema is generic; accepting a file name here would read as
	// "hookploy schema x.yaml validated x.yaml" and always exit 0.
	if len(pos) != 0 {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy schema   (takes no arguments; to check a file use `hookploy validate -f <file>`)")
		return 2
	}
	b, err := config.JSONSchema()
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "schema: %v\n", err)
		return 1
	}
	fmt.Fprintf(ctx.Stdout, "%s\n", b)
	return 0
}
