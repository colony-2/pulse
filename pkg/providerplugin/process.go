package providerplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/colony-2/pulse/pkg/compute"
	pb "github.com/colony-2/pulse/pkg/providerplugin/proto/v1"
	"github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Health is bounded host-owned diagnostics, never native errors or payloads.
type Health struct {
	Name           string `json:"name"`
	Implementation string `json:"implementation"`
	Version        string `json:"version"`
	Protocol       int    `json:"protocol"`
	State          string `json:"state"`
	Restarts       int    `json:"restart_count"`
	Generation     int    `json:"generation"`
	LastFailure    string `json:"last_failure,omitempty"`
}
type generation struct {
	cmd           *exec.Cmd
	client        *plugin.Client
	rpc           pb.InstanceProviderClient
	health        grpc_health_v1.HealthClient
	cancel        context.CancelFunc
	life          context.Context
	commandCancel context.CancelFunc
	parent        *os.File
	dir           string
	limit         int
}
type Process struct {
	mu      sync.Mutex
	cfg     ProcessConfig
	env     []string
	current *generation
	health  Health
	life    context.Context
	cancel  context.CancelFunc
	slots   chan struct{}
	calls   sync.WaitGroup
	done    chan struct{}
	once    sync.Once
}

// Start uses ctx only for initialization. Close owns the returned process.
func Start(ctx context.Context, c ProcessConfig) (*Process, error) {
	c = defaults(c)
	if ValidateConfig(c) != nil {
		return nil, ErrConfig
	}
	if c.BaseDir == "" {
		var e error
		c.BaseDir, e = os.Getwd()
		if e != nil {
			return nil, ErrConfig
		}
	}
	if !filepath.IsAbs(c.BaseDir) {
		return nil, ErrConfig
	}
	c.Args = append([]string{}, c.Args...)
	c.PassEnv = append([]string{}, c.PassEnv...)
	c.ConfigYAML = append([]byte{}, c.ConfigYAML...)
	c.RequiredFeatures = append([]string{}, c.RequiredFeatures...)
	life, cancel := context.WithCancel(context.Background())
	p := &Process{cfg: c, life: life, cancel: cancel, slots: make(chan struct{}, c.MaxConcurrent), done: make(chan struct{}), health: Health{Name: c.Name, State: "starting", Protocol: ProtocolVersion}}
	for _, k := range c.PassEnv {
		if v, ok := os.LookupEnv(k); ok {
			p.env = append(p.env, k+"="+v)
		}
	}
	g, info, e := p.startGeneration(ctx)
	if e != nil {
		cancel()
		return nil, e
	}
	p.current = g
	p.health.State = "ready"
	p.health.Generation = 1
	p.health.Implementation = info.Implementation
	p.health.Version = info.Version
	go p.supervise()
	return p, nil
}
func (p *Process) startGeneration(parent context.Context) (_ *generation, _ *pb.ConfigureResponse, err error) {
	ctx, cancel := context.WithTimeout(parent, p.cfg.StartTimeout)
	defer cancel()
	dir, e := os.MkdirTemp("", "pulse-provider-")
	if e != nil {
		return nil, nil, ErrInitialization
	}
	life, stop := context.WithCancel(p.life)
	commandLife, commandCancel := context.WithCancel(context.Background())
	g := &generation{dir: dir, life: life, cancel: stop, commandCancel: commandCancel, limit: p.cfg.MaxMessageBytes}
	defer func() {
		if err != nil {
			p.dispose(g, false)
		}
	}()
	reader, writer, e := os.Pipe()
	if e != nil {
		return nil, nil, ErrInitialization
	}
	defer reader.Close()
	g.parent = writer
	cmd := exec.CommandContext(commandLife, p.cfg.Command, p.cfg.Args...)
	isolateProcess(cmd)
	g.cmd = cmd
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, p.env...), "PULSE_PLUGIN_PARENT_FD=3")
	cmd.ExtraFiles = []*os.File{reader}
	cfg := &plugin.ClientConfig{HandshakeConfig: handshake, VersionedPlugins: pluginSet(nil), Cmd: cmd, AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC}, AutoMTLS: true, SkipHostEnv: true, StartTimeout: p.cfg.StartTimeout, Logger: hclog.NewNullLogger(), Stderr: io.Discard, SyncStdout: io.Discard, SyncStderr: io.Discard, GRPCDialOptions: []grpc.DialOption{grpc.WithDisableRetry(), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(g.limit), grpc.MaxCallSendMsgSize(g.limit))}}
	if p.cfg.SHA256 != "" {
		checksum, _ := hex.DecodeString(p.cfg.SHA256)
		cfg.SecureConfig = &plugin.SecureConfig{Checksum: checksum, Hash: sha256.New()}
	}
	g.client = plugin.NewClient(cfg)
	// Command lifetime is separate from initialization; the watcher kills only a
	// failed/timed-out startup and is joined before returning a ready generation.
	watched := make(chan struct{})
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		select {
		case <-ctx.Done():
			commandCancel()
		case <-watched:
		}
	}()
	defer func() { close(watched); <-watching }()
	client, e := g.client.Client()
	if e != nil || ctx.Err() != nil {
		return nil, nil, ErrInitialization
	}
	raw, e := client.Dispense(InterfaceName)
	if e != nil {
		return nil, nil, ErrInitialization
	}
	g.rpc = raw.(pb.InstanceProviderClient)
	if gc, ok := client.(*plugin.GRPCClient); ok {
		g.health = grpc_health_v1.NewHealthClient(gc.Conn)
	}
	info, e := g.rpc.Configure(ctx, &pb.ConfigureRequest{ProviderName: p.cfg.Name, ConfigYaml: p.cfg.ConfigYAML, BaseDir: p.cfg.BaseDir})
	if e != nil || ctx.Err() != nil || info == nil || info.MaxMessageBytes < MaxConfigBytes+1024 || info.MaxMessageBytes > MaxMessageBytes || !safeIdentity(info.Implementation) || !safeIdentity(info.Version) {
		return nil, nil, ErrInitialization
	}
	features := map[string]bool{}
	for _, f := range info.Features {
		features[f] = true
	}
	for _, f := range p.cfg.RequiredFeatures {
		if !features[f] {
			return nil, nil, ErrInitialization
		}
	}
	if int(info.MaxMessageBytes) < g.limit {
		g.limit = int(info.MaxMessageBytes)
	}
	return g, info, nil
}
func safeIdentity(s string) bool {
	return len(s) <= 128 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func (p *Process) dispose(g *generation, graceful bool) bool {
	if g == nil {
		return true
	}
	g.cancel()
	if graceful && g.rpc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ShutdownTimeout)
		_, _ = g.rpc.Shutdown(ctx, &pb.ShutdownRequest{})
		cancel()
	}
	// Force OS termination before Kill so a wedged RPC server cannot hold cleanup.
	g.commandCancel()
	terminateHelpers(g.cmd)
	if g.parent != nil {
		_ = g.parent.Close()
	}
	done := make(chan struct{})
	go func() {
		if g.client != nil {
			g.client.Kill()
		}
		_ = os.RemoveAll(g.dir)
		close(done)
	}()
	timer := time.NewTimer(p.cfg.ShutdownTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		p.mu.Lock()
		p.health.LastFailure = "cleanup_timeout"
		p.mu.Unlock()
		return false
	}
}
func (p *Process) Health() Health { p.mu.Lock(); defer p.mu.Unlock(); return p.health }
func (p *Process) supervise() {
	defer close(p.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	readySince := time.Now()
	attempts := 0
	lastPing := time.Now()
	for {
		select {
		case <-p.life.Done():
			return
		case <-ticker.C:
		}
		p.mu.Lock()
		g := p.current
		p.mu.Unlock()
		if g == nil {
			return
		}
		if !g.client.Exited() {
			if time.Since(readySince) >= 5*time.Minute {
				attempts = 0
			}
			if g.health != nil && time.Since(lastPing) >= 5*time.Second {
				ctx, cancel := context.WithTimeout(p.life, time.Second)
				_, e := g.health.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: plugin.GRPCServiceName})
				cancel()
				lastPing = time.Now()
				p.mu.Lock()
				if e != nil {
					p.health.LastFailure = "transport_unhealthy"
				}
				p.mu.Unlock()
			}
			continue
		}
		p.mu.Lock()
		if p.health.State == "stopping" {
			p.mu.Unlock()
			return
		}
		p.current = nil
		p.health.State = "restarting"
		p.health.LastFailure = "process_exit"
		p.mu.Unlock()
		if !p.dispose(g, false) {
			p.mu.Lock()
			if p.health.State != "stopping" {
				p.health.State = "failed"
			}
			p.mu.Unlock()
			return
		}
		for {
			if attempts >= 3 {
				p.mu.Lock()
				if p.health.State != "stopping" {
					p.health.State = "failed"
				}
				p.mu.Unlock()
				return
			}
			delay := time.Second * time.Duration(1<<attempts)
			delay += time.Duration(rand.Int64N(int64(delay / 4)))
			timer := time.NewTimer(delay)
			select {
			case <-p.life.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			attempts++
			p.mu.Lock()
			p.health.Restarts++
			p.mu.Unlock()
			next, info, e := p.startGeneration(p.life)
			p.mu.Lock()
			if p.health.State == "stopping" {
				p.mu.Unlock()
				p.dispose(next, false)
				return
			}
			if e != nil {
				p.health.LastFailure = "restart_initialization"
				p.mu.Unlock()
				continue
			}
			p.current = next
			p.health.Generation++
			p.health.State = "ready"
			p.health.Implementation = info.Implementation
			p.health.Version = info.Version
			p.mu.Unlock()
			readySince = time.Now()
			break
		}
	}
}

// acquire has no queue and never holds submissions for a replacement process.
func (p *Process) acquire(ctx context.Context) (*generation, context.Context, func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.health.State != "ready" || p.current == nil || ctx.Err() != nil || p.current.client.Exited() {
		return nil, nil, nil, false
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return nil, nil, nil, false
	}
	g := p.current
	call, cancel := context.WithTimeout(ctx, p.cfg.CallTimeout)
	stop := context.AfterFunc(g.life, cancel)
	p.calls.Add(1)
	return g, call, func() { stop(); cancel(); <-p.slots; p.calls.Done() }, true
}
func (p *Process) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	request, e := encodeLaunches(ls)
	if e != nil {
		return results(ls, compute.Rejected, "invalid launch batch"), ErrProtocol
	}
	if proto.Size(request) > p.cfg.MaxMessageBytes {
		return results(ls, compute.Rejected, "launch batch exceeds message limit"), ErrProtocol
	}
	g, call, done, ok := p.acquire(ctx)
	if !ok {
		return results(ls, compute.Unavailable, "provider unavailable before dispatch"), ErrUnavailable
	}
	defer done()
	if proto.Size(request) > g.limit {
		return results(ls, compute.Rejected, "launch batch exceeds message limit"), ErrProtocol
	}
	// From this point all transport errors are uncertain, including gRPC errors
	// that sound like safe declines. No application retry is ever performed.
	response, e := g.rpc.Submit(call, request, grpc.MaxCallRecvMsgSize(g.limit), grpc.MaxCallSendMsgSize(g.limit))
	if e != nil || response == nil {
		return results(ls, compute.Unknown, ErrUncertain.Error()), ErrUncertain
	}
	out, bad := normalizeResults(ls, response.Results)
	if bad || response.DiagnosticCode != "" {
		return out, ErrProtocol
	}
	return out, nil
}
func (p *Process) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	q, e := q.Normalize()
	if e != nil {
		return compute.ListResponse{}, ErrProtocol
	}
	g, call, done, ok := p.acquire(ctx)
	if !ok {
		return compute.ListResponse{}, ErrUnavailable
	}
	defer done()
	response, e := g.rpc.List(call, &pb.ListRequest{PageSize: int32(q.PageSize), PageToken: q.PageToken, LaunchId: q.LaunchID}, grpc.MaxCallRecvMsgSize(g.limit))
	if e != nil {
		return compute.ListResponse{}, ErrUnavailable
	}
	return decodeList(q, response)
}

// Close is idempotent and stops new calls before canceling the process lifetime.
func (p *Process) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.health.State = "stopping"
		p.cancel()
		p.mu.Unlock()
		<-p.done
		drained := make(chan struct{})
		go func() { p.calls.Wait(); close(drained) }()
		timer := time.NewTimer(p.cfg.ShutdownTimeout)
		select {
		case <-drained:
			timer.Stop()
		case <-timer.C:
		}
		p.mu.Lock()
		g := p.current
		p.current = nil
		p.mu.Unlock()
		p.dispose(g, true)
	})
}
