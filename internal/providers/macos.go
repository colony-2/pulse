package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	pulsevm "github.com/colony-2/pulse-tart"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/placement"
	"github.com/colony-2/pulse/internal/providerutil"
)

type macRuntime interface {
	api.Provider
	Check(context.Context) error
	Reconcile(context.Context) error
	Close() error
}

func macSettings(p config.Provider, baseDir string) (pulsevm.Config, pulsevm.VZConfig, error) {
	var c pulsevm.Config
	var local pulsevm.VZConfig
	options, err := config.MacOSOptions(p)
	if err != nil {
		return c, local, err
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
	c.OwnerID = options.OwnerID
	if c.OwnerID == "" {
		c.OwnerID = "server" // Match the default in the pulse-tart CLI.
	}
	c.BootstrapTimeout, err = duration(options.BootstrapTimeout)
	if err != nil {
		return c, local, err
	}
	c.VMCounts = pulsevm.VMCounts{MacOS: options.VMCounts.MacOS, Linux: options.VMCounts.Linux, Total: options.VMCounts.Total}
	knownBases := append(pulsevm.DefaultBases(), pulsevm.DefaultLinuxBase())
	password := func(image, username, env string) (pulsevm.Secret, error) {
		if env != "" {
			if value := os.Getenv(env); value != "" {
				return pulsevm.Secret(value), nil
			}
		} else {
			// Only library-published images may use their published login.
			for _, b := range knownBases {
				if image == b.Image && (username == "" || username == b.SSHUsername) {
					return b.SSHPassword, nil
				}
			}
		}
		return "", fmt.Errorf("VM image requires a populated ssh_password_env")
	}
	if options.Bases != nil {
		c.Bases = make([]pulsevm.Base, 0, len(options.Bases))
	}
	for _, b := range options.Bases {
		secret, e := password(b.Image, b.SSHUsername, b.SSHPasswordEnv)
		if e != nil {
			return c, local, e
		}
		key, e := read(b.SSHHostKeyFile)
		if e != nil {
			return c, local, fmt.Errorf("cannot read macOS SSH host key")
		}
		c.Bases = append(c.Bases, pulsevm.Base{Name: b.Name, OS: b.OS, Image: b.Image, OSVersion: b.OSVersion, SSHUsername: b.SSHUsername, SSHPassword: secret, SSHHostKey: key, Capabilities: b.Capabilities})
	}
	if options.Slots != nil {
		c.Slots = make([]pulsevm.SlotGroup, 0, len(options.Slots))
	}
	for _, s := range options.Slots {
		c.Slots = append(c.Slots, pulsevm.SlotGroup{Name: s.Name, Count: s.Count, Base: s.Base, CPU: s.CPU, MemoryMiB: s.MemoryMiB, DockerProfile: s.DockerProfile})
	}
	if d := options.Docker; d != nil {
		c.Docker = &pulsevm.DockerConfig{}
		c.Docker.ReadyTimeout, err = duration(d.ReadyTimeout)
		if err != nil {
			return c, local, err
		}
		if d.Profiles == nil && options.Slots != nil {
			c.Docker.Profiles = []pulsevm.DockerEngineProfile{pulsevm.DefaultDockerEngine()}
		}
		for _, profile := range d.Profiles {
			engine := pulsevm.DockerEngineProfile{Name: profile.Name, Image: profile.Image, SSHUsername: profile.SSHUsername, CPU: profile.CPU, MemoryMiB: profile.MemoryMiB, DiskGiB: profile.DiskGiB, EngineVersion: profile.EngineVersion}
			if engine.Image == "" {
				engine.Image = pulsevm.DefaultDockerEngine().Image
			}
			engine.SSHPassword, err = password(engine.Image, engine.SSHUsername, profile.SSHPasswordEnv)
			if err != nil {
				return c, local, err
			}
			engine.SSHHostKey, err = read(profile.SSHHostKeyFile)
			if err != nil {
				return c, local, fmt.Errorf("cannot read Docker engine SSH host key")
			}
			c.Docker.Profiles = append(c.Docker.Profiles, engine)
		}
	}
	if v := options.VZ; v != nil {
		local.Home = path(v.Home)
		local.Runner = path(v.Runner)
		local.DockerEndpoint = pulsevm.DockerEndpoint(v.DockerEndpoint)
		local.CommandTimeout, err = duration(v.CommandTimeout)
		if err != nil {
			return c, local, err
		}
		local.SSHTimeout, err = duration(v.SSHTimeout)
		if err != nil {
			return c, local, err
		}
		local.DeliveryTimeout, err = duration(v.DeliveryTimeout)
		if err != nil {
			return c, local, err
		}
	}
	// Leave omitted catalogs, pools, counts and resource sizes to the library.
	// Resolve here so startup reporting and maintenance use its effective values.
	c, err = pulsevm.ResolveConfig(c)
	if err != nil || options.Slots != nil || options.Docker != nil {
		return c, local, err
	}

	// Keep macOS Docker opt-in for Pulse's default slots. The library now enables
	// it on conventional slots, but macOS companions require a configured endpoint.
	linuxBases := make(map[string]bool)
	for _, base := range c.Bases {
		if base.OS == "linux" {
			linuxBases[base.Name] = true
		}
	}
	for i := range c.Slots {
		if !linuxBases[c.Slots[i].Base] {
			c.Slots[i].DockerProfile = ""
		}
	}
	if len(linuxBases) == 0 {
		c.Docker = nil
	}
	return c, local, nil
}

func buildMac(ctx context.Context, name string, p config.Provider, cfg *config.Config) (api.Provider, func(), error) {
	c, local, err := macSettings(p, cfg.BaseDir)
	if err != nil {
		return nil, nil, err
	}
	r, err := pulsevm.NewVZ(c, local)
	if err != nil {
		return nil, nil, fmt.Errorf("%s provider %s initialization failed: %w", p.Type, name, err)
	}
	if err = r.Check(ctx); err != nil {
		_ = r.Close()
		return nil, nil, fmt.Errorf("%s provider %s runtime check failed: %w", p.Type, name, err)
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
	vmCounts         pulsevm.VMCounts
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
	out, _, err := p.SubmitWithDiagnostics(ctx, launches)
	return out, err
}

func (p *macProvider) SubmitWithDiagnostics(ctx context.Context, launches []api.Launch) ([]api.Submission, map[string]placement.SubmissionDetail, error) {
	details := map[string]placement.SubmissionDetail{}
	var errs []error
	if err := providerutil.Launches(launches); err != nil {
		return nil, details, err
	}
	out := make([]api.Submission, 0, len(launches))
	// Reconcile can spend minutes cloning a cold image under the library's
	// state lock. Decline before dispatch instead of queuing a claimed lease.
	if !p.activity.TryRLock() {
		for _, launch := range launches {
			out = append(out, api.Submission{LaunchID: launch.LaunchID, Status: api.NoCapacity, Reason: "macOS slots are being reconciled"})
		}
		return out, details, errors.Join(errs...)
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
		detail := placement.SubmissionDetail{OriginalResults: []api.Submission{}, Error: placement.Error(err)}
		for _, response := range responses {
			if response.LaunchID == launch.LaunchID {
				detail.OriginalResults = append(detail.OriginalResults, response)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
		details[launch.LaunchID] = detail
		if len(responses) != 1 || responses[0].LaunchID != launch.LaunchID || !providerutil.ValidSubmission(responses[0]) {
			result = api.Submission{LaunchID: launch.LaunchID, Status: api.Unknown, Reason: "missing or invalid VZ submission result"}
			detail.NormalizationError = &result.Reason
			details[launch.LaunchID] = detail
			out = append(out, result)
			return out, details, errors.Join(errs...)
		}
		out = append(out, responses[0])
	}
	return out, details, errors.Join(errs...)
}

// Start warming only when the controller starts work, never during pulse check.
// Cold image preparation can outlast call_timeout; it must not hold a claimed
// lease or be interrupted by the controller's per-pass maintenance deadline.
func (p *macProvider) Maintain(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return pulsevm.ErrClosed
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
