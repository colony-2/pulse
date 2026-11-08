package providers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	tart "github.com/colony-2/pulse-tart"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providerutil"
)

type macRuntime interface {
	api.Provider
	Check(context.Context) error
	Reconcile(context.Context) error
	Close() error
}

func macSettings(name string, p config.Provider, baseDir string) (tart.Config, tart.TartConfig, tart.OrchardConfig, error) {
	var c tart.Config
	var local tart.TartConfig
	var remote tart.OrchardConfig
	options, err := config.MacOSOptions(p)
	if err != nil {
		return c, local, remote, err
	}
	path := func(s string) string {
		if s == "" || filepath.IsAbs(s) {
			return s
		}
		return filepath.Join(baseDir, s)
	}
	read := func(s string) ([]byte, error) {
		if s == "" {
			return nil, nil
		}
		return os.ReadFile(path(s))
	}
	duration := func(s string) (time.Duration, error) {
		if s == "" {
			return 0, nil
		}
		v, e := time.ParseDuration(s)
		if e != nil || v <= 0 {
			return 0, fmt.Errorf("invalid macOS provider duration")
		}
		return v, nil
	}
	identity := fmt.Sprintf("pulse-%s-%x", p.Type, sha256.Sum256([]byte(name)))[:32]
	c.OwnerID = options.OwnerID
	if c.OwnerID == "" {
		c.OwnerID = identity
	}
	c.StateDir = path(options.StateDir)
	if c.StateDir == "" {
		root, e := os.UserConfigDir()
		if e != nil {
			return c, local, remote, e
		}
		c.StateDir = filepath.Join(root, "pulse", "providers", identity)
	}
	c.BootstrapTimeout, err = duration(options.BootstrapTimeout)
	if err != nil {
		return c, local, remote, err
	}
	c.VMCounts = tart.VMCounts{MacOS: options.VMCounts.MacOS, Linux: options.VMCounts.Linux, Total: options.VMCounts.Total}
	knownBases := append(tart.DefaultBases(), tart.DefaultLinuxBase())
	password := func(image, username, env string) (tart.Secret, error) {
		if env != "" {
			if value := os.Getenv(env); value != "" {
				return tart.Secret(value), nil
			}
		} else {
			// Only library-published images may use their published login.
			for _, b := range knownBases {
				if image == b.Image && (username == "" || username == b.SSHUsername) {
					return b.SSHPassword, nil
				}
			}
		}
		return "", fmt.Errorf("Tart/Orchard image requires a populated ssh_password_env")
	}
	if options.Bases != nil {
		c.Bases = make([]tart.Base, 0, len(options.Bases))
	}
	for _, b := range options.Bases {
		secret, e := password(b.Image, b.SSHUsername, b.SSHPasswordEnv)
		if e != nil {
			return c, local, remote, e
		}
		key, e := read(b.SSHHostKeyFile)
		if e != nil {
			return c, local, remote, fmt.Errorf("cannot read macOS SSH host key")
		}
		c.Bases = append(c.Bases, tart.Base{Name: b.Name, OS: b.OS, Image: b.Image, OSVersion: b.OSVersion, SSHUsername: b.SSHUsername, SSHPassword: secret, SSHHostKey: key, Capabilities: b.Capabilities})
	}
	if options.Slots != nil {
		c.Slots = make([]tart.SlotGroup, 0, len(options.Slots))
	}
	for _, s := range options.Slots {
		c.Slots = append(c.Slots, tart.SlotGroup{Name: s.Name, Count: s.Count, Base: s.Base, CPU: s.CPU, MemoryMiB: s.MemoryMiB, DockerProfile: s.DockerProfile})
	}
	if d := options.Docker; d != nil {
		c.Docker = &tart.DockerConfig{}
		c.Docker.ReadyTimeout, err = duration(d.ReadyTimeout)
		if err != nil {
			return c, local, remote, err
		}
		if d.Profiles == nil && options.Slots != nil {
			c.Docker.Profiles = []tart.DockerEngineProfile{tart.DefaultDockerEngine()}
		}
		for _, profile := range d.Profiles {
			engine := tart.DockerEngineProfile{Name: profile.Name, Image: profile.Image, SSHUsername: profile.SSHUsername, CPU: profile.CPU, MemoryMiB: profile.MemoryMiB, DiskGiB: profile.DiskGiB, EngineVersion: profile.EngineVersion}
			if engine.Image == "" {
				engine.Image = tart.DefaultDockerEngine().Image
			}
			engine.SSHPassword, err = password(engine.Image, engine.SSHUsername, profile.SSHPasswordEnv)
			if err != nil {
				return c, local, remote, err
			}
			engine.SSHHostKey, err = read(profile.SSHHostKeyFile)
			if err != nil {
				return c, local, remote, fmt.Errorf("cannot read Docker engine SSH host key")
			}
			c.Docker.Profiles = append(c.Docker.Profiles, engine)
		}
	}
	if t := options.Tart; t != nil {
		local.Binary, local.Home = t.Binary, path(t.Home)
		local.DockerEndpoint = tart.DockerEndpoint(t.DockerEndpoint)
		local.PythonBinary = t.PythonBinary
		if local.PythonBinary != "" && filepath.Base(local.PythonBinary) != local.PythonBinary {
			local.PythonBinary = path(local.PythonBinary)
		}
		if local.Binary != "" && filepath.Base(local.Binary) != local.Binary {
			local.Binary = path(local.Binary)
		}
		local.CommandTimeout, err = duration(t.CommandTimeout)
		if err != nil {
			return c, local, remote, err
		}
		local.SSHTimeout, err = duration(t.SSHTimeout)
		if err != nil {
			return c, local, remote, err
		}
	}
	if o := options.Orchard; o != nil {
		token := os.Getenv(o.TokenEnv)
		if o.TokenEnv != "" && token == "" {
			return c, local, remote, fmt.Errorf("Orchard token_env is empty")
		}
		remote = tart.OrchardConfig{URL: o.URL, Worker: o.Worker, WorkerLabels: o.WorkerLabels, Username: o.Username, Token: tart.Secret(token), AllowInsecureHTTP: o.AllowHTTP}
		remote.DockerEndpoint = tart.DockerEndpoint(o.DockerEndpoint)
		remote.CACert, err = read(o.CACertFile)
		if err != nil {
			return c, local, remote, fmt.Errorf("cannot read Orchard CA certificate")
		}
		remote.RequestTimeout, err = duration(o.RequestTimeout)
	}
	if err != nil {
		return c, local, remote, err
	}
	// Leave omitted catalogs, pools, counts and resource sizes to the library.
	// Resolve here so startup reporting and maintenance use its effective values.
	c, err = tart.ResolveConfig(c)
	return c, local, remote, err
}

func buildMac(ctx context.Context, name string, p config.Provider, cfg *config.Config) (api.Provider, func(), error) {
	c, local, remote, err := macSettings(name, p, cfg.BaseDir)
	if err != nil {
		return nil, nil, err
	}
	var r *tart.Runtime
	if p.Type == "tart" {
		r, err = tart.NewTart(c, local)
	} else {
		r, err = tart.NewOrchard(c, remote)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s provider %s initialization failed: %w", p.Type, name, err)
	}
	if err = r.Check(ctx); err != nil {
		_ = r.Close()
		return nil, nil, fmt.Errorf("%s provider %s connectivity check failed: %w", p.Type, name, err)
	}
	host := &macProvider{runtime: r, vmCounts: c.VMCounts, bootstrapTimeout: c.BootstrapTimeout, envUnsupported: len(cfg.Defaults.Env) != 0}
	for _, slot := range c.Slots {
		host.slots += slot.Count
	}
	return host, host.close, nil
}

// The library owns VM lifecycle, leases, slots and durable state. This host shim
// schedules reconciliation independently of claims. Catalog discovery and base
// selection belong to the provider library.
type macProvider struct {
	runtime          macRuntime
	slots            int
	vmCounts         tart.VMCounts
	bootstrapTimeout time.Duration
	envUnsupported   bool
	activity         sync.RWMutex
	mu               sync.Mutex
	cancel           context.CancelFunc
	done             chan struct{}
	closed           bool
	lastError        error
}

func (p *macProvider) List(ctx context.Context, q api.ListRequest) (api.ListResponse, error) {
	return p.runtime.List(ctx, q)
}
func (p *macProvider) GetCapabilities(ctx context.Context) (api.Capabilities, error) {
	return p.runtime.GetCapabilities(ctx)
}
func (p *macProvider) Submit(ctx context.Context, launches []api.Launch) ([]api.Submission, error) {
	if err := providerutil.Launches(launches); err != nil {
		return nil, err
	}
	out := make([]api.Submission, 0, len(launches))
	// Reconcile can spend minutes cloning a cold image under the library's
	// journal lock. Decline before dispatch instead of queuing a claimed lease.
	if !p.activity.TryRLock() {
		for _, launch := range launches {
			out = append(out, api.Submission{LaunchID: launch.LaunchID, Status: api.NoCapacity, Reason: "macOS slots are being reconciled"})
		}
		return out, nil
	}
	defer p.activity.RUnlock()
	for _, launch := range launches {
		result := api.Submission{LaunchID: launch.LaunchID, Status: api.Unsupported}
		if p.envUnsupported {
			result.Reason = "macOS provider does not support defaults.env"
			out = append(out, result)
			continue
		}
		responses, err := p.runtime.Submit(ctx, []api.Launch{launch})
		if len(responses) != 1 || responses[0].LaunchID != launch.LaunchID || !providerutil.ValidSubmission(responses[0]) {
			result = api.Submission{LaunchID: launch.LaunchID, Status: api.Unknown, Reason: "missing or invalid Tart/Orchard submission result"}
			out = append(out, result)
			return out, err
		}
		out = append(out, responses[0])
	}
	return out, nil
}

// Start warming only when the controller starts work, never during pulse check.
// Cold image preparation can outlast call_timeout; it must not hold a claimed
// lease or be interrupted by the controller's per-pass maintenance deadline.
func (p *macProvider) Maintain(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return tart.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.cancel == nil {
		lifetime, cancel := context.WithCancel(context.Background())
		p.cancel, p.done = cancel, make(chan struct{})
		go p.reconcile(lifetime)
	}
	return p.lastError
}
func (p *macProvider) reconcile(ctx context.Context) {
	defer close(p.done)
	for {
		call, cancel := context.WithTimeout(ctx, p.bootstrapTimeout)
		p.activity.Lock()
		err := p.runtime.Reconcile(call)
		p.activity.Unlock()
		cancel()
		p.mu.Lock()
		p.lastError = err
		p.mu.Unlock()
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (p *macProvider) close() {
	p.mu.Lock()
	p.closed = true
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	_ = p.runtime.Close()
}
