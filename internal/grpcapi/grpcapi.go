// Package grpcapi is the main-side gRPC layer: it authenticates edge
// sessions, tracks their online state, and exposes each session as an
// executor.Executor that ships executions over the stream and routes
// progress updates back into engine.Sinks.
package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/pb"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
	"github.com/reorx/hookploy/internal/version"
)

// Server implements the Hookploy gRPC service on main.
type Server struct {
	pb.UnimplementedHookployServer
	Store    *store.Store
	Registry *executor.Registry
	Config   func() *config.Config
	Logger   *log.Logger

	mu    sync.Mutex
	edges map[string]*session
}

// Edges snapshots the currently connected edge sessions.
func (s *Server) Edges() map[string]model.EdgeInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]model.EdgeInfo, len(s.edges))
	for name, sess := range s.edges {
		out[name] = sess.info
	}
	return out
}

func (s *Server) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

// Session is the single bidirectional stream: authenticate the Hello, then
// serve as this edge's executor until the stream dies.
func (s *Server) Session(stream pb.Hookploy_SessionServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be hello")
	}
	rec, err := s.Store.LookupToken(token.Hash(hello.Token))
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	// The server name comes from the token's subject; a name in Hello is
	// only an assertion that must agree.
	if rec == nil || rec.Kind != string(token.KindServer) ||
		(hello.Server != "" && hello.Server != rec.Subject) {
		s.logf("edge %q rejected: invalid server token", hello.Server)
		return status.Error(codes.Unauthenticated, "invalid server token")
	}
	name := rec.Subject
	if s.Config().Servers[name] == nil {
		s.logf("edge %q rejected: not declared in config", name)
		return status.Errorf(codes.PermissionDenied, "server %q is not declared in hookploy.yaml", name)
	}

	sess := &session{
		stream:   stream,
		inflight: map[string]*inflight{},
		info: model.EdgeInfo{
			Server:      name,
			Version:     hello.Version,
			ConnectedAt: time.Now(),
		},
	}
	if err := sess.send(&pb.MainMessage{Msg: &pb.MainMessage_Ack{Ack: &pb.HelloAck{MainVersion: version.Version, Server: name}}}); err != nil {
		return err
	}

	s.mu.Lock()
	if s.edges == nil {
		s.edges = map[string]*session{}
	}
	s.edges[name] = sess
	s.mu.Unlock()
	s.Registry.Register(name, sess)
	s.logf("edge %q connected (version %s)", name, hello.Version)

	defer func() {
		s.mu.Lock()
		if s.edges[name] == sess {
			delete(s.edges, name)
		}
		s.mu.Unlock()
		s.Registry.Unregister(name, sess)
		sess.failAll(fmt.Errorf("edge %q disconnected", name))
		s.logf("edge %q disconnected", name)
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil // edge going away is a normal end of session
		}
		if u := msg.GetUpdate(); u != nil {
			sess.dispatch(u)
		}
	}
}

// session is one live edge stream; it is the executor for its server.
type session struct {
	stream pb.Hookploy_SessionServer
	info   model.EdgeInfo

	sendMu sync.Mutex // grpc streams allow one concurrent sender

	mu       sync.Mutex
	inflight map[string]*inflight
}

type execResult struct {
	ok         bool
	errMsg     string
	digest     string
	sessionErr error // stream-level failure (disconnect)
}

type inflight struct {
	sink engine.Sink
	done chan execResult
}

func (sess *session) send(msg *pb.MainMessage) error {
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	return sess.stream.Send(msg)
}

// Execute ships the spec to the edge and blocks until its terminal update,
// stream death, or ctx expiry (the scheduler's timeout backstop — the edge
// enforces the same timeout locally).
func (sess *session) Execute(ctx context.Context, spec engine.Spec, sink engine.Sink) (engine.Result, error) {
	res := engine.Result{Digest: spec.Digest}
	opsJSON, err := json.Marshal(spec.Steps)
	if err != nil {
		return res, fmt.Errorf("marshal ops: %w", err)
	}
	infl := &inflight{sink: sink, done: make(chan execResult, 1)}
	sess.mu.Lock()
	sess.inflight[spec.ExecutionID] = infl
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		delete(sess.inflight, spec.ExecutionID)
		sess.mu.Unlock()
	}()

	err = sess.send(&pb.MainMessage{Msg: &pb.MainMessage_Exec{Exec: &pb.Execution{
		ExecutionId: spec.ExecutionID,
		Kind:        spec.Kind,
		Service:     spec.Service,
		Instance:    spec.Instance,
		Dir:         spec.Dir,
		Image:       spec.Image,
		Digest:      spec.Digest,
		OpsJson:     opsJSON,
		TimeoutMs:   spec.Timeout.Milliseconds(),
	}}})
	if err != nil {
		return res, fmt.Errorf("send execution to edge: %w", err)
	}

	select {
	case r := <-infl.done:
		if r.digest != "" {
			res.Digest = r.digest
		}
		if r.sessionErr != nil {
			return res, r.sessionErr
		}
		if !r.ok {
			if r.errMsg == "" {
				r.errMsg = "execution failed on edge"
			}
			return res, errors.New(r.errMsg)
		}
		return res, nil
	case <-ctx.Done():
		return res, ctx.Err()
	}
}

// dispatch routes one update to its execution's sink. Updates for unknown
// executions (finished, timed out on main) are dropped.
func (sess *session) dispatch(u *pb.ExecUpdate) {
	sess.mu.Lock()
	infl := sess.inflight[u.ExecutionId]
	sess.mu.Unlock()
	if infl == nil {
		return
	}
	switch ev := u.Event.(type) {
	case *pb.ExecUpdate_OpStart:
		infl.sink.OpStart(int(ev.OpStart.Index), ev.OpStart.Name)
	case *pb.ExecUpdate_OpEnd:
		var exit *int
		if ev.OpEnd.ExitCode != nil {
			v := int(*ev.OpEnd.ExitCode)
			exit = &v
		}
		var opErr error
		if ev.OpEnd.Error != "" {
			opErr = errors.New(ev.OpEnd.Error)
		}
		infl.sink.OpEnd(int(ev.OpEnd.Index), ev.OpEnd.Name, exit, opErr)
	case *pb.ExecUpdate_Log:
		infl.sink.Log(int(ev.Log.Index), ev.Log.Stream, string(ev.Log.Data))
	case *pb.ExecUpdate_Done:
		select {
		case infl.done <- execResult{ok: ev.Done.Ok, errMsg: ev.Done.Error, digest: ev.Done.Digest}:
		default:
		}
	}
}

// failAll aborts every in-flight execution when the session dies.
func (sess *session) failAll(err error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	for _, infl := range sess.inflight {
		select {
		case infl.done <- execResult{sessionErr: err}:
		default:
		}
	}
}
