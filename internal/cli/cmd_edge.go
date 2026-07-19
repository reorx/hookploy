package cli

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/reorx/hookploy/internal/edge"
)

func cmdEdge(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("edge", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	mainURL := fs.String("main", "", "main URL, e.g. https://hookploy.example.com")
	tok := fs.String("token", "", "server token (hps_...); or HOOKPLOY_SERVER_TOKEN")
	server := fs.String("server", "", "optional server name assertion (derived from the token by default)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tok == "" {
		*tok = os.Getenv("HOOKPLOY_SERVER_TOKEN")
	}
	if *mainURL == "" || *tok == "" {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy edge --main <url> --token <t> [--server <name>]")
		return 2
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	err := edge.Run(runCtx, edge.Options{
		MainURL: *mainURL,
		Token:   *tok,
		Server:  *server,
		Logger:  log.New(ctx.Stderr, "", log.LstdFlags),
	})
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "edge: %v\n", err)
		return 1
	}
	return 0
}
