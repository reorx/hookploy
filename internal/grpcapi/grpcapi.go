// Package grpcapi is the main-side gRPC adapter: it authenticates edge
// sessions, translates between the protobuf wire and engine types, and hands
// the session to internal/edgehub, which owns all transport-agnostic
// behaviour (attachment state, executors, update routing).
package grpcapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/edgehub"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/pb"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
	"github.com/reorx/hookploy/internal/version"
)

// transportName tags gRPC attachments in EdgeInfo and logs.
const transportName = "grpc"

// Server implements the Hookploy gRPC service on main.
type Server struct {
	pb.UnimplementedHookployServer
	Store  *store.Store
	Config func() *config.Config
	Logger *log.Logger

	// Hub is the shared edge hub. When nil one is built from Registry on
	// first use, which keeps gRPC-only setups a single-field construction.
	Hub *edgehub.Hub
	// Registry is only consulted to build that fallback hub.
	Registry *executor.Registry

	once sync.Once
}

func (s *Server) hub() *edgehub.Hub {
	s.once.Do(func() {
		if s.Hub == nil {
			s.Hub = edgehub.New(s.Registry, s.Logger)
		}
	})
	return s.Hub
}

// Edges snapshots the currently connected edge sessions.
func (s *Server) Edges() map[string]model.EdgeInfo {
	return s.hub().Edges()
}

func (s *Server) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

// Session is the single bidirectional stream: authenticate the Hello, attach
// to the hub, then pump updates back into it until the stream dies.
func (s *Server) Session(stream pb.Hookploy_SessionServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be hello")
	}
	name, err := s.authenticate(hello)
	if err != nil {
		return err
	}

	conn := &grpcConn{stream: stream, kick: make(chan struct{})}
	if err := conn.send(&pb.MainMessage{Msg: &pb.MainMessage_Ack{Ack: &pb.HelloAck{
		MainVersion: version.Version, Server: name,
	}}}); err != nil {
		return err
	}

	att := s.hub().Attach(edgehub.AttachInfo{
		Server:    name,
		Version:   hello.Version,
		Transport: transportName,
	}, conn)
	defer att.Detach()

	return s.pump(stream, conn, name)
}

// authenticate resolves the edge's server name from its token. The name
// comes from the token's subject; a name in Hello is only an assertion that
// must agree.
func (s *Server) authenticate(hello *pb.Hello) (string, error) {
	rec, err := s.Store.LookupToken(token.Hash(hello.Token))
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	if rec == nil || rec.Kind != string(token.KindServer) ||
		(hello.Server != "" && hello.Server != rec.Subject) {
		s.logf("edge %q rejected: invalid server token", hello.Server)
		return "", status.Error(codes.Unauthenticated, "invalid server token")
	}
	name := rec.Subject
	if s.Config().Servers[name] == nil {
		s.logf("edge %q rejected: not declared in config", name)
		return "", status.Errorf(codes.PermissionDenied, "server %q is not declared in hookploy.yaml", name)
	}
	return name, nil
}

// pump reads the edge's updates in a helper goroutine so the handler can
// also wake on a kick: returning from the handler is what closes a stream
// the hub has replaced, and a blocked Recv would otherwise pin it open.
func (s *Server) pump(stream pb.Hookploy_SessionServer, conn *grpcConn, name string) error {
	updates := make(chan *pb.ExecUpdate)
	go func() {
		defer close(updates)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			u := msg.GetUpdate()
			if u == nil {
				continue
			}
			select {
			case updates <- u:
			case <-conn.kick:
				return
			}
		}
	}()
	for {
		select {
		case u, ok := <-updates:
			if !ok {
				return nil // edge going away is a normal end of session
			}
			s.route(name, u)
		case <-conn.kick:
			return nil // a newer session took this server over
		}
	}
}

// route decodes one update and forwards it to the hub.
func (s *Server) route(server string, u *pb.ExecUpdate) {
	h := s.hub()
	switch ev := u.Event.(type) {
	case *pb.ExecUpdate_OpStart:
		h.HandleOpStart(server, u.ExecutionId, int(ev.OpStart.Index), ev.OpStart.Name)
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
		h.HandleOpEnd(server, u.ExecutionId, int(ev.OpEnd.Index), ev.OpEnd.Name, exit, opErr)
	case *pb.ExecUpdate_Log:
		h.HandleLog(server, u.ExecutionId, int(ev.Log.Index), ev.Log.Stream, string(ev.Log.Data))
	case *pb.ExecUpdate_Done:
		h.HandleDone(server, u.ExecutionId, ev.Done.Ok, ev.Done.Error, ev.Done.Digest)
	}
}

// grpcConn is the hub's downstream view of one edge stream.
type grpcConn struct {
	stream pb.Hookploy_SessionServer

	sendMu sync.Mutex // grpc streams allow one concurrent sender
	kick   chan struct{}
	once   sync.Once
}

func (c *grpcConn) send(msg *pb.MainMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(msg)
}

func (c *grpcConn) SendExecution(spec engine.Spec) error {
	opsJSON, err := json.Marshal(spec.Steps)
	if err != nil {
		return fmt.Errorf("marshal ops: %w", err)
	}
	return c.send(&pb.MainMessage{Msg: &pb.MainMessage_Exec{Exec: &pb.Execution{
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
}

// Close makes the handler return, which tears the stream down.
func (c *grpcConn) Close() {
	c.once.Do(func() { close(c.kick) })
}
