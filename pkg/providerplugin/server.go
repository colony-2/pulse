package providerplugin

import (
	"context"
	"github.com/colony-2/pulse/pkg/compute"
	pb "github.com/colony-2/pulse/pkg/providerplugin/proto/v1"
	"github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const InterfaceName = "instance_provider"

var handshake = plugin.HandshakeConfig{ProtocolVersion: ProtocolVersion, MagicCookieKey: "PULSE_PLUGIN_COOKIE", MagicCookieValue: "pulse-instance-provider-v1"}

type rpcPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	server *server
}

func (p *rpcPlugin) GRPCServer(_ *plugin.GRPCBroker, g *grpc.Server) error {
	pb.RegisterInstanceProviderServer(g, p.server)
	return nil
}
func (*rpcPlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return pb.NewInstanceProviderClient(c), nil
}
func pluginSet(s *server) map[int]plugin.PluginSet {
	return map[int]plugin.PluginSet{ProtocolVersion: {InterfaceName: &rpcPlugin{server: s}}}
}

// Serve reserves stdout for the framework. Provider diagnostics must never
// contain configuration, credentials, stdin, or raw native API errors.
func Serve(factory Factory, options ...ServeOptions) {
	o := ServeOptions{}
	if len(options) > 0 {
		o = options[0]
	}
	if o.MaxMessageBytes == 0 {
		o.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if o.MaxMessageBytes < MaxConfigBytes+1024 || o.MaxMessageBytes > MaxMessageBytes {
		panic("invalid provider message limit")
	}
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &server{factory: factory, options: o, life: life, cancel: cancel}
	// A dedicated inherited pipe closes even on abrupt host death. Framework
	// transport reconnects cannot keep an orphaned provider alive.
	if value := os.Getenv("PULSE_PLUGIN_PARENT_FD"); value != "" {
		fd, e := strconv.Atoi(value)
		if e != nil || fd < 3 {
			panic("invalid parent descriptor")
		}
		parent := os.NewFile(uintptr(fd), "pulse-parent")
		go func() {
			_, _ = io.Copy(io.Discard, parent)
			cancel()
			ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			done := make(chan struct{})
			go func() { _, _ = s.Shutdown(ctx, &pb.ShutdownRequest{}); close(done) }()
			select {
			case <-done:
			case <-ctx.Done():
			}
			exitWithHelpers()
		}()
	}
	defer func() {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		s.Shutdown(ctx, &pb.ShutdownRequest{})
	}()
	plugin.Serve(&plugin.ServeConfig{HandshakeConfig: handshake, VersionedPlugins: pluginSet(s), Logger: hclog.NewNullLogger(), GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
		return grpc.NewServer(append(opts, grpc.MaxRecvMsgSize(o.MaxMessageBytes), grpc.MaxSendMsgSize(o.MaxMessageBytes))...)
	}})
}

type server struct {
	pb.UnimplementedInstanceProviderServer
	mu        sync.RWMutex
	attempted bool
	stopping  bool
	provider  compute.Provider
	factory   Factory
	options   ServeOptions
	life      context.Context
	cancel    context.CancelFunc
}

func (s *server) Configure(ctx context.Context, r *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	s.mu.Lock()
	if s.attempted || s.stopping {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "provider already configured")
	}
	s.attempted = true
	s.mu.Unlock()
	if r == nil || !filepath.IsAbs(r.BaseDir) || ValidateYAML(r.ConfigYaml) != nil {
		return nil, status.Error(codes.InvalidArgument, ErrInitialization.Error())
	}
	p, e := s.factory(ctx, Init{Name: r.ProviderName, ConfigYAML: append([]byte{}, r.ConfigYaml...), BaseDir: r.BaseDir, Lifetime: s.life})
	if e != nil || p == nil || ctx.Err() != nil {
		s.cancel()
		return nil, status.Error(codes.FailedPrecondition, ErrInitialization.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return nil, status.Error(codes.Unavailable, ErrUnavailable.Error())
	}
	s.provider = p
	return &pb.ConfigureResponse{Implementation: s.options.Implementation, Version: s.options.Version, Features: s.options.Features, MaxMessageBytes: int64(s.options.MaxMessageBytes)}, nil
}
func (s *server) ready() (compute.Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.provider == nil || s.stopping {
		return nil, status.Error(codes.FailedPrecondition, ErrUnavailable.Error())
	}
	return s.provider, nil
}
func (s *server) Submit(ctx context.Context, r *pb.SubmitRequest) (*pb.SubmitResponse, error) {
	p, e := s.ready()
	if e != nil {
		return nil, e
	}
	ls, e := decodeLaunches(r)
	if e != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid launches")
	}
	rs, e := p.Submit(ctx, ls)
	out, bad := encodeResults(ls, rs)
	code := ""
	if e != nil || bad {
		code = "provider_error"
	}
	return &pb.SubmitResponse{Results: out, DiagnosticCode: code}, nil
}
func (s *server) List(ctx context.Context, r *pb.ListRequest) (*pb.ListResponse, error) {
	p, e := s.ready()
	if e != nil {
		return nil, e
	}
	q, e := (compute.ListRequest{PageSize: int(r.GetPageSize()), PageToken: r.GetPageToken(), LaunchID: r.GetLaunchId()}).Normalize()
	if e != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid list request")
	}
	v, e := p.List(ctx, q)
	if e != nil {
		return nil, status.Error(codes.Unavailable, "provider listing failed")
	}
	out, e := encodeList(q, v)
	if e != nil {
		return nil, status.Error(codes.Internal, ErrProtocol.Error())
	}
	return out, nil
}
func (s *server) Shutdown(ctx context.Context, _ *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return &pb.ShutdownResponse{}, nil
	}
	s.stopping = true
	s.cancel()
	s.mu.Unlock()
	if s.options.Shutdown != nil {
		if s.options.Shutdown(ctx) != nil {
			return nil, status.Error(codes.Internal, "provider shutdown failed")
		}
	}
	return &pb.ShutdownResponse{}, nil
}
