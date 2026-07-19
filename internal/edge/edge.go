// Package edge is the stateless executor role: it dials out to main, keeps
// one gRPC session alive with exponential-backoff reconnects, runs the
// executions main sends through the local op engine, and streams progress
// back. It has zero local configuration — everything arrives with the task.
package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/pb"
	"github.com/reorx/hookploy/internal/runner"
	"github.com/reorx/hookploy/internal/version"
)

// Options configures an edge.
type Options struct {
	MainURL string // http(s)://host[:port] — https dials TLS (Caddy h2), http is plaintext h2c
	Token   string // server token (hps_...)
	Server  string // optional identity assertion; main derives the name from the token

	Engine      *engine.Engine // nil → real runner + HTTP client
	Logger      *log.Logger
	BackoffBase time.Duration // reconnect backoff start, default 1s
	BackoffMax  time.Duration // reconnect backoff cap, default 30s
}

// Run connects to main and serves executions until ctx is canceled. It only
// returns early on unrecoverable setup errors (bad URL); connection failures
// are retried forever with exponential backoff.
func Run(ctx context.Context, opts Options) error {
	target, creds, err := dialTarget(opts.MainURL)
	if err != nil {
		return err
	}
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

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}))
	if err != nil {
		return fmt.Errorf("dial %s: %w", opts.MainURL, err)
	}
	defer conn.Close()
	client := pb.NewHookployClient(conn)

	backoff := base
	for {
		ok := runSession(ctx, client, opts, eng, logger)
		if ctx.Err() != nil {
			return nil
		}
		if ok {
			backoff = base // the session was established; start over gently
		}
		logger.Printf("reconnecting in %s", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		backoff *= 2
		if backoff > max {
			backoff = max
		}
	}
}

// dialTarget parses the main URL into a gRPC target and credentials.
func dialTarget(mainURL string) (string, credentials.TransportCredentials, error) {
	u, err := url.Parse(mainURL)
	if err != nil {
		return "", nil, fmt.Errorf("--main %q: %w", mainURL, err)
	}
	host := u.Host
	switch u.Scheme {
	case "https":
		if u.Port() == "" {
			host += ":443"
		}
		return host, credentials.NewTLS(nil), nil
	case "http":
		if u.Port() == "" {
			host += ":80"
		}
		return host, insecure.NewCredentials(), nil
	default:
		return "", nil, fmt.Errorf("--main %q: scheme must be http or https", mainURL)
	}
}

// runSession runs one connect→hello→serve cycle. Returns whether the
// session got past the handshake (for backoff reset).
func runSession(ctx context.Context, client pb.HookployClient, opts Options, eng *engine.Engine, logger *log.Logger) bool {
	// sessCtx cancels every in-flight execution when the session dies:
	// main marks them failed on disconnect, so keeping them running here
	// would only let the two sides diverge.
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := client.Session(sessCtx)
	if err != nil {
		logger.Printf("connect: %v", err)
		return false
	}
	err = stream.Send(&pb.EdgeMessage{Msg: &pb.EdgeMessage_Hello{Hello: &pb.Hello{
		Server:  opts.Server,
		Token:   opts.Token,
		Version: version.Version,
	}}})
	if err != nil {
		logger.Printf("hello: %v", err)
		return false
	}
	first, err := stream.Recv()
	if err != nil {
		logger.Printf("handshake rejected: %v", err)
		return false
	}
	ack := first.GetAck()
	if ack == nil {
		logger.Printf("handshake: expected ack, got %T", first.Msg)
		return false
	}
	logger.Printf("connected to main %s as server %q", ack.MainVersion, ack.Server)

	sess := &session{stream: stream, eng: eng, logger: logger}
	for {
		msg, err := stream.Recv()
		if err != nil {
			if ctx.Err() == nil {
				logger.Printf("session lost: %v", err)
			}
			return true
		}
		if exec := msg.GetExec(); exec != nil {
			go sess.runExecution(sessCtx, exec)
		}
	}
}

type session struct {
	stream pb.Hookploy_SessionClient
	eng    *engine.Engine
	logger *log.Logger
	sendMu sync.Mutex
}

func (s *session) send(u *pb.ExecUpdate) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	// A dead stream shows up as Recv errors in the session loop; updates
	// lost here are re-covered by main failing the execution on disconnect.
	_ = s.stream.Send(&pb.EdgeMessage{Msg: &pb.EdgeMessage_Update{Update: u}})
}

// runExecution executes one spec and reports its terminal state. The
// timeout is enforced here: the engine's runner kills the process group
// when the deadline passes.
func (s *session) runExecution(ctx context.Context, exec *pb.Execution) {
	done := func(ok bool, errMsg, digest string) {
		s.send(&pb.ExecUpdate{ExecutionId: exec.ExecutionId, Event: &pb.ExecUpdate_Done{Done: &pb.ExecDone{
			Ok: ok, Error: errMsg, Digest: digest,
		}}})
	}
	var steps []ops.Step
	if err := json.Unmarshal(exec.OpsJson, &steps); err != nil {
		done(false, "edge cannot decode ops (version mismatch?): "+err.Error(), "")
		return
	}
	s.logger.Printf("execution %s: %s/%s starting (%d ops)", exec.ExecutionId, exec.Service, exec.Instance, len(steps))
	if exec.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(exec.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	res, err := s.eng.Execute(ctx, engine.Spec{
		ExecutionID: exec.ExecutionId,
		Kind:        exec.Kind,
		Service:     exec.Service,
		Instance:    exec.Instance,
		Dir:         exec.Dir,
		Image:       exec.Image,
		Digest:      exec.Digest,
		Steps:       steps,
	}, &streamSink{sess: s, execID: exec.ExecutionId})
	if err != nil {
		s.logger.Printf("execution %s failed: %v", exec.ExecutionId, err)
		done(false, err.Error(), res.Digest)
		return
	}
	s.logger.Printf("execution %s succeeded", exec.ExecutionId)
	done(true, "", res.Digest)
}

// streamSink forwards engine progress over the session stream.
type streamSink struct {
	sess   *session
	execID string
}

func (w *streamSink) OpStart(i int, name string) {
	w.sess.send(&pb.ExecUpdate{ExecutionId: w.execID, Event: &pb.ExecUpdate_OpStart{OpStart: &pb.OpStart{
		Index: int32(i), Name: name,
	}}})
}

func (w *streamSink) OpEnd(i int, name string, exit *int, err error) {
	msg := &pb.OpEnd{Index: int32(i), Name: name}
	if exit != nil {
		v := int32(*exit)
		msg.ExitCode = &v
	}
	if err != nil {
		msg.Error = err.Error()
	}
	w.sess.send(&pb.ExecUpdate{ExecutionId: w.execID, Event: &pb.ExecUpdate_OpEnd{OpEnd: msg}})
}

func (w *streamSink) Log(i int, stream, data string) {
	w.sess.send(&pb.ExecUpdate{ExecutionId: w.execID, Event: &pb.ExecUpdate_Log{Log: &pb.LogChunk{
		Index: int32(i), Stream: stream, Data: []byte(data),
	}}})
}
