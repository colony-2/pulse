package providerplugin

import (
	"context"
	"fmt"
	"github.com/colony-2/pulse/pkg/compute"
	pb "github.com/colony-2/pulse/pkg/providerplugin/proto/v1"
	"github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
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

// Serve reserves stdout for the framework. Submission method errors are captured
// for operator placement diagnostics; do not add configuration, stdin,
// or request payloads to diagnostics merely to supply an error.
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
	mu            sync.RWMutex
	attempted     bool
	stopping      bool
	provider      compute.Provider
	factory       Factory
	options       ServeOptions
	responseLimit int
	life          context.Context
	cancel        context.CancelFunc
}

func (s *server) Configure(ctx context.Context, r *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	s.mu.Lock()
	if s.attempted || s.stopping {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "provider already configured")
	}
	s.attempted = true
	s.mu.Unlock()
	if r == nil || !filepath.IsAbs(r.BaseDir) || ValidateYAML(r.ConfigYaml) != nil || (r.MaxMessageBytes != 0 && (r.MaxMessageBytes < MaxConfigBytes+1024 || r.MaxMessageBytes > MaxMessageBytes)) {
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
	s.responseLimit = s.options.MaxMessageBytes
	if s.responseLimit == 0 {
		s.responseLimit = DefaultMaxMessageBytes
	}
	if r.MaxMessageBytes != 0 {
		s.responseLimit = min(s.responseLimit, int(r.MaxMessageBytes))
	}
	return &pb.ConfigureResponse{Implementation: s.options.Implementation, Version: s.options.Version, Features: s.options.Features, MaxMessageBytes: int64(s.responseLimit)}, nil
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
	response := &pb.SubmitResponse{Results: out, DiagnosticCode: code}
	if e != nil {
		attachSubmissionError(response, e.Error(), s.responseLimit)
	}
	attachSubmissionObservations(response, ls, rs, s.responseLimit)
	return response, nil
}

// Prioritize the existing authoritative results within the negotiated message
// bound. Newly transported method text uses the remaining room; an oversize
// error is explicitly marked rather than turning acceptance into RPC failure.
func attachSubmissionError(response *pb.SubmitResponse, raw string, limit int) {
	budget := limit - proto.Size(response)
	if 1+protowire.SizeBytes(len(raw)) <= budget {
		response.ProviderError = []byte(raw)
		return
	}
	// 128 bytes cover the field tag/length and marker at the 128 MiB maximum.
	keep := max(0, min(len(raw), budget-128))
	if utf8.ValidString(raw) {
		for keep > 0 && keep < len(raw) && !utf8.RuneStart(raw[keep]) {
			keep--
		}
	}
	response.ProviderError = []byte(raw[:keep] + fmt.Sprintf("\n[truncated after %d bytes; provider error exceeds negotiated response limit]", keep))
}

// Preserve matching duplicates and malformed Reasons without relaxing admission
// validation. Diagnostics use only space left by results and the method error.
func attachSubmissionObservations(response *pb.SubmitResponse, ls []compute.Launch, rs []compute.Submission, limit int) {
	ids := map[string]bool{}
	for _, l := range ls {
		ids[l.LaunchID] = true
	}
	const truncated = "original provider results truncated at negotiated response limit"
	observed := &pb.SubmissionObservations{CaptureError: truncated}
	response.Observations = observed
	if proto.Size(response) > limit {
		response.Observations = nil
		return
	}
	for _, r := range rs {
		if !ids[r.LaunchID] {
			continue
		}
		item := &pb.OriginalSubmission{LaunchId: r.LaunchID, Status: []byte(r.Status), Reason: []byte(r.Reason)}
		observed.Results = append(observed.Results, item)
		if proto.Size(response) > limit {
			// Preserve the prefix of this Reason if it fits. The separate capture
			// error explicitly covers this item and any remaining observations.
			raw := item.Reason
			item.Reason = nil
			budget := limit - proto.Size(response) - 16
			if budget >= 0 {
				keep := min(len(raw), budget)
				if utf8.Valid(raw) {
					for keep > 0 && keep < len(raw) && !utf8.RuneStart(raw[keep]) {
						keep--
					}
				}
				item.Reason = raw[:keep]
			} else {
				observed.Results = observed.Results[:len(observed.Results)-1]
			}
			return
		}
	}
	observed.CaptureError = ""
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
