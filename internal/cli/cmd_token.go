package cli

import (
	"flag"
	"fmt"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

// Token commands run on the main host only and operate on SQLite directly
// (WAL allows coexistence with a running main). The plaintext token is
// printed exactly once, to stdout.

// tokenArgs is the parsed common form of every token command.
type tokenArgs struct {
	Subject string
	Config  *config.Config
	Store   *store.Store
	JSON    bool
}

// parseTokenArgs extracts one positional subject plus -f and --json.
func parseTokenArgs(ctx *Context, name string, args []string, needSubject bool) (ta tokenArgs, code int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	file := fs.String("f", "hookploy.yaml", "path to hookploy.yaml")
	asJSON := fs.Bool("json", false, "output JSON")
	rest, ok := parseInterleaved(fs, args)
	if !ok {
		return ta, 2
	}
	if needSubject {
		if len(rest) != 1 {
			fmt.Fprintf(ctx.Stderr, "usage: hookploy %s <name> [-f hookploy.yaml] [--json]\n", name)
			return ta, 2
		}
		ta.Subject = rest[0]
	} else if len(rest) != 0 {
		fmt.Fprintf(ctx.Stderr, "usage: hookploy %s [-f hookploy.yaml] [--json]\n", name)
		return ta, 2
	}
	ta.JSON = *asJSON
	cfg, err := config.Load(*file)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "config: %v\n", err)
		return ta, 1
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return ta, 1
	}
	ta.Config, ta.Store = cfg, st
	return ta, 0
}

// printToken emits the freshly minted plaintext — once, to stdout.
func printToken(ctx *Context, kind token.Kind, subject, plain string, asJSON bool) int {
	if asJSON {
		if err := printJSON(ctx, api.TokenCreated{Kind: string(kind), Subject: subject, Token: plain}); err != nil {
			return jsonWriteFail(ctx, err)
		}
		return 0
	}
	if _, err := fmt.Fprintln(ctx.Stdout, plain); err != nil {
		fmt.Fprintf(ctx.Stderr, "error: writing token to stdout: %v\n", err)
		return 1
	}
	return 0
}

func cmdTokenCreate(ctx *Context, args []string) int {
	ta, code := parseTokenArgs(ctx, "token create", args, true)
	if code != 0 {
		return code
	}
	defer ta.Store.Close()
	if ta.Config.Services[ta.Subject] == nil {
		fmt.Fprintf(ctx.Stderr, "unknown service %q (not in %s)\n", ta.Subject, ta.Config.Path)
		return 1
	}
	plain := token.New(token.KindService)
	if err := ta.Store.InsertToken(string(token.KindService), ta.Subject, token.Hash(plain)); err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	return printToken(ctx, token.KindService, ta.Subject, plain, ta.JSON)
}

func cmdTokenRotate(ctx *Context, args []string) int {
	ta, code := parseTokenArgs(ctx, "token rotate", args, true)
	if code != 0 {
		return code
	}
	defer ta.Store.Close()
	if ta.Config.Services[ta.Subject] == nil {
		fmt.Fprintf(ctx.Stderr, "unknown service %q (not in %s)\n", ta.Subject, ta.Config.Path)
		return 1
	}
	plain := token.New(token.KindService)
	if err := ta.Store.RotateToken(string(token.KindService), ta.Subject, token.Hash(plain)); err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	return printToken(ctx, token.KindService, ta.Subject, plain, ta.JSON)
}

func cmdTokenRevoke(ctx *Context, args []string) int {
	ta, code := parseTokenArgs(ctx, "token revoke", args, true)
	if code != 0 {
		return code
	}
	defer ta.Store.Close()
	n, err := ta.Store.RevokeTokens(string(token.KindService), ta.Subject)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	if ta.JSON {
		if err := printJSON(ctx, api.TokenRevoked{Kind: string(token.KindService), Subject: ta.Subject, Revoked: n > 0}); err != nil {
			return jsonWriteFail(ctx, err)
		}
		return 0
	}
	fmt.Fprintf(ctx.Stdout, "revoked %d token(s) of service %s\n", n, ta.Subject)
	return 0
}

func cmdServerToken(ctx *Context, args []string) int {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy server token create <server> [-f hookploy.yaml] [--json]")
		return 2
	}
	ta, code := parseTokenArgs(ctx, "server token create", args[1:], true)
	if code != 0 {
		return code
	}
	defer ta.Store.Close()
	if ta.Config.Servers[ta.Subject] == nil {
		fmt.Fprintf(ctx.Stderr, "unknown server %q (not in %s)\n", ta.Subject, ta.Config.Path)
		return 1
	}
	plain := token.New(token.KindServer)
	if err := ta.Store.InsertToken(string(token.KindServer), ta.Subject, token.Hash(plain)); err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	return printToken(ctx, token.KindServer, ta.Subject, plain, ta.JSON)
}

func cmdAdminTokenCreate(ctx *Context, args []string) int {
	ta, code := parseTokenArgs(ctx, "admin-token create", args, false)
	if code != 0 {
		return code
	}
	defer ta.Store.Close()
	plain := token.New(token.KindAdmin)
	if err := ta.Store.InsertToken(string(token.KindAdmin), "admin", token.Hash(plain)); err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	return printToken(ctx, token.KindAdmin, "admin", plain, ta.JSON)
}
