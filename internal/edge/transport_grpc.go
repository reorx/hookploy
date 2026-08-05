package edge

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/reorx/hookploy/internal/pb"
	"github.com/reorx/hookploy/internal/version"
)

// grpcTransport dials main's bidirectional stream. The gRPC wire carries
// neither a done ack nor an inflight list in Hello, so its sessions are not
// resumable: executions die with the stream, as they always have.
type grpcTransport struct {
	client pb.HookployClient
	conn   *grpc.ClientConn
	token  string
	server string
}

// newGRPCTransport builds the long-lived client connection; individual
// sessions are streams on top of it.
func newGRPCTransport(opts Options) (*grpcTransport, error) {
	target, creds, err := dialTarget(opts.MainURL)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", opts.MainURL, err)
	}
	return &grpcTransport{
		client: pb.NewHookployClient(conn),
		conn:   conn,
		token:  opts.Token,
		server: opts.Server,
	}, nil
}

func (t *grpcTransport) Close() { _ = t.conn.Close() }

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

func (t *grpcTransport) Dial(ctx context.Context, _ []string) (Session, error) {
	stream, err := t.client.Session(ctx)
	if err != nil {
		return nil, err
	}
	err = stream.Send(&pb.EdgeMessage{Msg: &pb.EdgeMessage_Hello{Hello: &pb.Hello{
		Server:  t.server,
		Token:   t.token,
		Version: version.Version,
	}}})
	if err != nil {
		return nil, fmt.Errorf("hello: %w", err)
	}
	first, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("handshake rejected: %w", err)
	}
	ack := first.GetAck()
	if ack == nil {
		return nil, fmt.Errorf("handshake: expected ack, got %T", first.Msg)
	}
	return &grpcSession{stream: stream, mainVersion: ack.MainVersion, server: ack.Server}, nil
}

type grpcSession struct {
	stream      pb.Hookploy_SessionClient
	mainVersion string
	server      string
}

func (s *grpcSession) Recv() (*Task, error) {
	for {
		msg, err := s.stream.Recv()
		if err != nil {
			return nil, err
		}
		exec := msg.GetExec()
		if exec == nil {
			continue
		}
		return &Task{
			ExecutionID: exec.ExecutionId,
			Kind:        exec.Kind,
			Service:     exec.Service,
			Instance:    exec.Instance,
			Dir:         exec.Dir,
			Image:       exec.Image,
			Digest:      exec.Digest,
			OpsJSON:     exec.OpsJson,
			Timeout:     time.Duration(exec.TimeoutMs) * time.Millisecond,
		}, nil
	}
}

func (s *grpcSession) send(u *pb.ExecUpdate) error {
	return s.stream.Send(&pb.EdgeMessage{Msg: &pb.EdgeMessage_Update{Update: u}})
}

func (s *grpcSession) ReportUpdate(execID string, u Update) {
	// A dead stream shows up as a Recv error in the agent loop; updates lost
	// here are re-covered by main failing the execution on disconnect.
	_ = s.send(wireUpdate(execID, u))
}

func (s *grpcSession) ReportDone(execID string, d DoneReport) error {
	// The gRPC protocol has no done ack: a successful send is the strongest
	// confirmation available.
	return s.send(&pb.ExecUpdate{ExecutionId: execID, Event: &pb.ExecUpdate_Done{Done: &pb.ExecDone{
		Ok: d.OK, Error: d.Error, Digest: d.Digest,
	}}})
}

func (s *grpcSession) Resumable() bool     { return false }
func (s *grpcSession) MainVersion() string { return s.mainVersion }
func (s *grpcSession) Server() string      { return s.server }
func (s *grpcSession) Close()              { _ = s.stream.CloseSend() }

func wireUpdate(execID string, u Update) *pb.ExecUpdate {
	switch u.Kind {
	case UpdateOpStart:
		return &pb.ExecUpdate{ExecutionId: execID, Event: &pb.ExecUpdate_OpStart{OpStart: &pb.OpStart{
			Index: int32(u.Index), Name: u.Name,
		}}}
	case UpdateOpEnd:
		msg := &pb.OpEnd{Index: int32(u.Index), Name: u.Name, Error: u.Error}
		if u.ExitCode != nil {
			v := int32(*u.ExitCode)
			msg.ExitCode = &v
		}
		return &pb.ExecUpdate{ExecutionId: execID, Event: &pb.ExecUpdate_OpEnd{OpEnd: msg}}
	default:
		return &pb.ExecUpdate{ExecutionId: execID, Event: &pb.ExecUpdate_Log{Log: &pb.LogChunk{
			Index: int32(u.Index), Stream: u.Stream, Data: []byte(u.Data),
		}}}
	}
}
