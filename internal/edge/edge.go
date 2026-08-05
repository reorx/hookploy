// Package edge is the stateless executor role: it dials out to main, keeps
// one session alive with exponential-backoff reconnects, runs the executions
// main sends through the local op engine, and streams progress back. It has
// zero local configuration — everything arrives with the task.
package edge

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/runner"
)

// Transport names. Both speak to the same main on the same host; grpc uses
// the dedicated gRPC port, sse rides the ordinary HTTP listener.
const (
	TransportGRPC = "grpc"
	TransportSSE  = "sse"
)

// Options configures an edge.
type Options struct {
	MainURL string // http(s)://host[:port] — https dials TLS (Caddy h2), http is plaintext h2c
	Token   string // server token (hps_...)
	Server  string // optional identity assertion; main derives the name from the token
	// Transport selects the wire: grpc (default) or sse. There is no
	// auto-detection and no fallback — silently switching would hide a
	// misconfiguration, and the two are not interchangeable in what they
	// tolerate.
	Transport string

	Engine      *engine.Engine // nil → real runner + HTTP client
	Logger      *log.Logger
	BackoffBase time.Duration // reconnect backoff start, default 1s
	BackoffMax  time.Duration // reconnect backoff cap, default 30s
}

// Run connects to main and serves executions until ctx is canceled. It only
// returns early on unrecoverable setup errors (bad URL); connection failures
// are retried forever with exponential backoff.
func Run(ctx context.Context, opts Options) error {
	logger := opts.Logger
	if logger == nil {
		logger = log.New(log.Writer(), "", log.LstdFlags)
	}
	eng := opts.Engine
	if eng == nil {
		eng = &engine.Engine{
			Runner: &runner.ExecRunner{},
			HTTP:   &http.Client{Timeout: 5 * time.Minute},
		}
	}
	base := opts.BackoffBase
	if base <= 0 {
		base = time.Second
	}
	max := opts.BackoffMax
	if max <= 0 {
		max = 30 * time.Second
	}

	tr, closeTr, err := newTransport(ctx, opts, logger)
	if err != nil {
		return err
	}
	defer closeTr()

	newAgent(eng, logger, base, max).run(ctx, tr)
	return nil
}

// newTransport builds the configured wire. The returned closer releases
// long-lived connection state.
func newTransport(ctx context.Context, opts Options, logger *log.Logger) (Transport, func(), error) {
	switch opts.Transport {
	case "", TransportGRPC:
		tr, err := newGRPCTransport(opts)
		if err != nil {
			return nil, nil, err
		}
		return tr, tr.Close, nil
	case TransportSSE:
		tr, err := newSSETransport(ctx, opts, logger)
		if err != nil {
			return nil, nil, err
		}
		return tr, func() {}, nil
	default:
		return nil, nil, fmt.Errorf("--transport %q: must be %s or %s", opts.Transport, TransportGRPC, TransportSSE)
	}
}
