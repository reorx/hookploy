package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/edgehub"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/grpcapi"
	"github.com/reorx/hookploy/internal/httpapi"
	"github.com/reorx/hookploy/internal/notify"
	"github.com/reorx/hookploy/internal/pb"
	"github.com/reorx/hookploy/internal/runner"
	"github.com/reorx/hookploy/internal/scheduler"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/webui"
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

	notifier := notify.New(st, func() *config.Config { return cfgVal.Load() }, logger)
	notifier.Start()

	sched := scheduler.New(st, reg)
	// Wired after Recover on purpose: closing out rollouts the previous
	// process abandoned is not news, and Recover runs before the listeners
	// are even up.
	if err := sched.Recover(); err != nil {
		fmt.Fprintf(ctx.Stderr, "recover: %v\n", err)
		return 1
	}
	sched.Notify = notifier.Notify

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

	// One hub across every edge transport: it owns attachment state and the
	// per-server executors, so Edges() is already the merged view.
	hub := edgehub.New(reg, logger)
	grpcSrv := &grpcapi.Server{
		Store:  st,
		Hub:    hub,
		Config: func() *config.Config { return cfgVal.Load() },
		Logger: logger,
	}
	apiSrv := &httpapi.Server{
		Store:  st,
		Sched:  sched,
		Config: func() *config.Config { return cfgVal.Load() },
		Reload: reload,
		// A deploy that fails to build never reaches the scheduler, so the
		// HTTP layer reports that one itself.
		Notify: notifier.Notify,
		Edges:  hub.Edges,
		// Serving the SSE edge transport on the HTTP listener: same host,
		// same port, no gRPC-aware proxy needed in front.
		Hub: hub,
	}
	var ui *webui.Server
	if cfg.WebUI {
		ui = webui.New(st, func() *config.Config { return cfgVal.Load() }, hub.Edges)
		apiSrv.SessionOK = ui.SessionValid
	} else {
		logger.Printf("web ui disabled (webui: false)")
	}
	httpServer := &http.Server{Addr: cfg.Listen.HTTP, Handler: mainHandler(apiSrv, ui)}

	// gRPC listener for edges (h2c; Caddy terminates TLS in front). The
	// keepalive pair detects dead edge connections within ~40s.
	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	pb.RegisterHookployServer(grpcServer, grpcSrv)
	grpcLis, err := net.Listen("tcp", cfg.Listen.GRPC)
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "grpc listen: %v\n", err)
		return 1
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("hookploy main listening on http://%s grpc://%s (db: %s)", cfg.Listen.HTTP, cfg.Listen.GRPC, cfg.DB)
		errCh <- httpServer.ListenAndServe()
	}()
	go func() {
		if err := grpcServer.Serve(grpcLis); err != nil {
			logger.Printf("grpc: %v", err)
		}
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
			grpcServer.Stop()
			sched.Shutdown()
			// After the scheduler: shutting it down settles the rollouts it
			// was holding, and those notifications are worth the short wait.
			notifier.Shutdown()
			logger.Printf("bye")
			return 0
		}
	}
}

// mainHandler assembles main's HTTP routes: the webhook/admin API always; the
// web UI and the / → /ui/ redirect only when ui is non-nil (`webui: true`).
func mainHandler(apiSrv *httpapi.Server, ui *webui.Server) http.Handler {
	mux := http.NewServeMux()
	if ui != nil {
		mux.Handle("/ui/", ui.Handler())
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusFound)
		})
	}
	mux.Handle("/", apiSrv.Handler())
	return mux
}
