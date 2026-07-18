package cli

import (
	"flag"
	"fmt"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

// Token commands run on the main host only and operate on SQLite directly
// (WAL allows coexistence with a running main). The plaintext token is
// printed exactly once, to stdout.

// parseTokenArgs extracts one positional subject plus -f.
func parseTokenArgs(ctx *Context, name string, args []string, needSubject bool) (subject string, cfg *config.Config, st *store.Store, code int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	file := fs.String("f", "hookploy.yaml", "path to hookploy.yaml")
	rest, ok := parseInterleaved(fs, args)
	if !ok {
		return "", nil, nil, 2
	}
	if needSubject {
		if len(rest) != 1 {
			fmt.Fprintf(ctx.Stderr, "usage: hookploy %s <name> [-f hookploy.yaml]\n", name)
			return "", nil, nil, 2
		}
		subject = rest[0]
	} else if len(rest) != 0 {
		fmt.Fprintf(ctx.Stderr, "usage: hookploy %s [-f hookploy.yaml]\n", name)
		return "", nil, nil, 2
	}
	cfg, err := config.Load(*file)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "config: %v\n", err)
		return "", nil, nil, 1
	}
	st, err = store.Open(cfg.DB)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return "", nil, nil, 1
	}
	return subject, cfg, st, 0
}

func cmdTokenCreate(ctx *Context, args []string) int {
	svc, cfg, st, code := parseTokenArgs(ctx, "token create", args, true)
	if code != 0 {
		return code
	}
	defer st.Close()
	if cfg.Services[svc] == nil {
		fmt.Fprintf(ctx.Stderr, "unknown service %q (not in %s)\n", svc, cfg.Path)
		return 1
	}
	plain := token.New(token.KindService)
	if err := st.InsertToken(string(token.KindService), svc, token.Hash(plain)); err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	fmt.Fprintln(ctx.Stdout, plain)
	return 0
}

func cmdTokenRotate(ctx *Context, args []string) int {
	svc, cfg, st, code := parseTokenArgs(ctx, "token rotate", args, true)
	if code != 0 {
		return code
	}
	defer st.Close()
	if cfg.Services[svc] == nil {
		fmt.Fprintf(ctx.Stderr, "unknown service %q (not in %s)\n", svc, cfg.Path)
		return 1
	}
	plain := token.New(token.KindService)
	if err := st.RotateToken(string(token.KindService), svc, token.Hash(plain)); err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	fmt.Fprintln(ctx.Stdout, plain)
	return 0
}

func cmdTokenRevoke(ctx *Context, args []string) int {
	svc, _, st, code := parseTokenArgs(ctx, "token revoke", args, true)
	if code != 0 {
		return code
	}
	defer st.Close()
	n, err := st.RevokeTokens(string(token.KindService), svc)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	fmt.Fprintf(ctx.Stdout, "revoked %d token(s) of service %s\n", n, svc)
	return 0
}

func cmdServerToken(ctx *Context, args []string) int {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy server token create <server> [-f hookploy.yaml]")
		return 2
	}
	server, cfg, st, code := parseTokenArgs(ctx, "server token create", args[1:], true)
	if code != 0 {
		return code
	}
	defer st.Close()
	if cfg.Servers[server] == nil {
		fmt.Fprintf(ctx.Stderr, "unknown server %q (not in %s)\n", server, cfg.Path)
		return 1
	}
	plain := token.New(token.KindServer)
	if err := st.InsertToken(string(token.KindServer), server, token.Hash(plain)); err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	fmt.Fprintln(ctx.Stdout, plain)
	return 0
}

func cmdAdminTokenCreate(ctx *Context, args []string) int {
	_, _, st, code := parseTokenArgs(ctx, "admin-token create", args, false)
	if code != 0 {
		return code
	}
	defer st.Close()
	plain := token.New(token.KindAdmin)
	if err := st.InsertToken(string(token.KindAdmin), "admin", token.Hash(plain)); err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	fmt.Fprintln(ctx.Stdout, plain)
	return 0
}
