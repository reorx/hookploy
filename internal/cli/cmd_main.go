package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/httpapi"
	"github.com/reorx/hookploy/internal/runner"
	"github.com/reorx/hookploy/internal/scheduler"
	"github.com/reorx/hookploy/internal/store"
)

// acquireWindow is how long a dispatching execution waits for its server's
// executor (PRD: 30s edge reconnect grace).
const acquireWindow = 30 * time.Second

func cmdMain(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("main", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	file := fs.String("f", "hookploy.yaml", "path to hookploy.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := log.New(ctx.Stderr, "", log.LstdFlags)

	cfg, err := config.Load(*file)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "config: %v\n", err)
		return 1
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "store: %v\n", err)
		return 1
	}
	defer st.Close()

	var cfgVal atomic.Pointer[config.Config]
	cfgVal.Store(cfg)

	reg := executor.NewRegistry(acquireWindow)
	local := &executor.Local{Engine: &engine.Engine{
		Runner: &runner.ExecRunner{},
		HTTP:   &http.Client{Timeout: 5 * time.Minute},
	}}
	registerLocals := func(c *config.Config) {
		for name, srv := range c.Servers {
			if srv.Local {
				reg.Register(name, local)
			}
		}
	}
	registerLocals(cfg)

	sched := scheduler.New(st, reg)
	if err := sched.Recover(); err != nil {
		fmt.Fprintf(ctx.Stderr, "recover: %v\n", err)
		return 1
	}

	reload := func() error {
		c2, err := config.Load(*file)
		if err != nil {
			return err
		}
		cfgVal.Store(c2)
		registerLocals(c2)
		logger.Printf("config reloaded: %d servers, %d services", len(c2.Servers), len(c2.Services))
		return nil
	}

	apiSrv := &httpapi.Server{
		Store:  st,
		Sched:  sched,
		Config: func() *config.Config { return cfgVal.Load() },
		Reload: reload,
	}
	httpServer := &http.Server{Addr: cfg.Listen.HTTP, Handler: apiSrv.Handler()}

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("hookploy main listening on http://%s (db: %s)", cfg.Listen.HTTP, cfg.DB)
		errCh <- httpServer.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(ctx.Stderr, "http: %v\n", err)
				return 1
			}
			return 0
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				if err := reload(); err != nil {
					logger.Printf("reload failed, keeping previous config: %v", err)
				}
				continue
			}
			logger.Printf("received %s, shutting down", sig)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = httpServer.Shutdown(shutdownCtx)
			cancel()
			sched.Shutdown()
			logger.Printf("bye")
			return 0
		}
	}
}
