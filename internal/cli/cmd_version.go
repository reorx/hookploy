package cli

import (
	"fmt"

	"github.com/reorx/hookploy/internal/version"
)

func cmdVersion(ctx *Context, args []string) int {
	fmt.Fprintf(ctx.Stdout, "hookploy %s\n", version.Version)
	return 0
}
