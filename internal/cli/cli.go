// Package cli implements the hookploy command-line interface with a
// hand-written two-level command router on top of the standard flag package.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

// command is one first-level subcommand. Either Run is set, or Sub holds a
// second-level command table.
type command struct {
	Name    string
	Summary string
	Run     func(ctx *Context, args []string) int
	Sub     map[string]*command
}

// Context carries the output streams into command implementations.
type Context struct {
	Stdout io.Writer
	Stderr io.Writer
}

var commands []*command

func register(c *command) { commands = append(commands, c) }

func find(name string) *command {
	for _, c := range commands {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Run is the CLI entry point. It returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	ctx := &Context{Stdout: stdout, Stderr: stderr}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printUsage(stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] == "--version" || args[0] == "-v" {
		args[0] = "version"
	}
	c := find(args[0])
	if c == nil {
		fmt.Fprintf(stderr, "hookploy: unknown command %q\nRun 'hookploy --help' for usage.\n", args[0])
		return 2
	}
	rest := args[1:]
	if c.Sub != nil {
		if len(rest) == 0 {
			fmt.Fprintf(stderr, "hookploy %s: missing subcommand (one of:", c.Name)
			for name := range c.Sub {
				fmt.Fprintf(stderr, " %s", name)
			}
			fmt.Fprintln(stderr, ")")
			return 2
		}
		sub, ok := c.Sub[rest[0]]
		if !ok {
			fmt.Fprintf(stderr, "hookploy %s: unknown subcommand %q\n", c.Name, rest[0])
			return 2
		}
		return sub.Run(ctx, rest[1:])
	}
	return c.Run(ctx, rest)
}

// parseInterleaved parses flags allowing positionals and flags in any order
// (the flag package alone stops at the first positional). Returns the
// positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, bool) {
	var positional []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return nil, false
		}
		rest = fs.Args()
		if len(rest) > 0 {
			positional = append(positional, rest[0])
			rest = rest[1:]
		}
	}
	return positional, true
}

// printJSON writes v as indented JSON to stdout — the --json form of the
// local commands, whose DTOs live in internal/api just like the remote ones.
// The write error is returned rather than swallowed: `token create` prints its
// plaintext exactly once, so a lost write is a lost secret.
func printJSON(ctx *Context, v any) error {
	enc := json.NewEncoder(ctx.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// jsonWriteFail reports a failed --json write. stdout may be a closed pipe or
// a full disk; exiting 0 would claim success for output nobody received.
func jsonWriteFail(ctx *Context, err error) int {
	fmt.Fprintf(ctx.Stderr, "error: writing JSON output: %v\n", err)
	return 1
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "hookploy — centralized declarative webhook deployer")
	fmt.Fprintln(w, "\nUsage: hookploy <command> [arguments]")
	fmt.Fprintln(w, "\nCommands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-14s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w, "\nRemote commands read HOOKPLOY_URL and HOOKPLOY_ADMIN_TOKEN from the environment.")
}
