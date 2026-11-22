package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pulsevm "github.com/colony-2/pulse-macvz"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providerutil"
)

type macFake struct {
	calls     []api.Launch
	outcomes  []api.Status
	reconcile func(context.Context) error
	closed    atomic.Bool
	invalid   bool
}

func (f *macFake) Submit(_ context.Context, ls []api.Launch, _ api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	f.calls = append(f.calls, ls...)
	if f.invalid {
		return nil, nil, errors.New("uncertain")
	}
	status := api.Accepted
	if len(f.outcomes) > 0 {
		status, f.outcomes = f.outcomes[0], f.outcomes[1:]
	}
	r := api.Submission{LaunchID: ls[0].LaunchID, Status: status}
	if status != api.Accepted {
		r.Reason = "test outcome"
	}
	return []api.Submission{r}, nil, nil
}
func (f *macFake) List(context.Context, api.ListRequest) (api.ListResponse, error) {
	return api.ListResponse{Items: []api.Instance{}}, nil
}
func (f *macFake) Check(context.Context) error       { return nil }
func (f *macFake) Reconcile(c context.Context) error { return f.reconcile(c) }
func (f *macFake) Close() error                      { f.closed.Store(true); return nil }
func macLaunch(base string) api.Launch {
	return api.Launch{LaunchID: "test", Allocation: api.Allocation{Base: base, Capabilities: []string{"git"}}, Job: api.JobRef{JobDB: "https://db/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}, Lease: "exact-lease"}
}
func TestMacForwardsSelectorsAndAssignments(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		status     api.Status
		invalid    bool
	}{
		{"middle wildcard", "macos-*arm64", api.Accepted, false},
		{"new provider base", "future-os*", api.Accepted, false},
		{"linux docker", "ubuntu-*", api.Accepted, false},
		{"unsupported", "darwin*", api.Unsupported, false},
		{"capacity", "macos*", api.NoCapacity, false},
		{"unknown", "macos*", api.Unknown, false},
		{"unavailable", "macos*", api.Unavailable, false},
		{"malformed", "macos*", api.Unknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &macFake{outcomes: []api.Status{tc.status}, invalid: tc.invalid}
			p := &macProvider{runtime: f}
			launch := macLaunch(tc.base)
			launch.Capabilities = []string{"docker", "git"}
			out, _, _ := p.Submit(t.Context(), []api.Launch{launch}, api.SubmitOptions{})
			if len(out) != 1 || out[0].Status != tc.status || len(f.calls) != 1 {
				t.Fatal(out, f.calls)
			}
			if !reflect.DeepEqual(f.calls[0], launch) {
				t.Fatal("assignment changed")
			}
		})
	}
	f := &macFake{}
	p := &macProvider{runtime: f, envUnsupported: true}
	out, _, err := p.Submit(t.Context(), []api.Launch{macLaunch("macos*")}, api.SubmitOptions{})
	if err != nil || out[0].Status != api.Unsupported || len(f.calls) != 0 {
		t.Fatal("environment ignored", out, err)
	}
	launch := macLaunch("macos*")
	if _, _, err = p.Submit(t.Context(), []api.Launch{launch, launch}, api.SubmitOptions{}); err == nil {
		t.Fatal("duplicate batch accepted")
	}
}
func TestMacMaintenanceOutlivesCallAndStopsOnClose(t *testing.T) {
	entered := make(chan context.Context, 1)
	f := &macFake{reconcile: func(ctx context.Context) error { entered <- ctx; <-ctx.Done(); return ctx.Err() }}
	p := &macProvider{runtime: f, bootstrapTimeout: time.Minute}
	call, cancel := context.WithCancel(t.Context())
	if err := p.Maintain(call); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case ctx := <-entered:
		if ctx.Err() != nil {
			t.Fatal("call deadline cancelled preparation")
		}
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not start")
	}
	out, _, err := p.Submit(t.Context(), []api.Launch{macLaunch("macos*")}, api.SubmitOptions{})
	if err != nil || out[0].Status != api.NoCapacity || len(f.calls) != 0 {
		t.Fatal("lease queued behind cold preparation", out, err)
	}
	p.close()
	if !f.closed.Load() {
		t.Fatal("runtime not closed")
	}
	if p.Maintain(t.Context()) != pulsevm.ErrClosed {
		t.Fatal("closed runtime restarted")
	}
}
func (*macFake) GetCapabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{}, nil
}

func TestMacDefaultsComeFromLibrary(t *testing.T) {
	c, _, err := macSettings(config.Provider{Type: "vz"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.OwnerID != "server" {
		t.Fatalf("owner = %q, want server", c.OwnerID)
	}
	want, err := pulsevm.ResolveConfig(pulsevm.Config{OwnerID: c.OwnerID})
	if err != nil {
		t.Fatal(err)
	}
	for i := range want.Slots {
		if want.Slots[i].Base != pulsevm.DefaultLinuxBase().Name {
			want.Slots[i].DockerProfile = ""
		}
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatal("Pulse replaced library conventions", err)
	}
	if c.NeedsDockerEndpoint() || c.BootstrapTimeout == 0 {
		t.Fatal("ordinary defaults need a Docker endpoint or lost preparation timeout")
	}
}

func TestMacDockerDefaultsKeepOptIn(t *testing.T) {
	base := pulsevm.DefaultBases()[0]
	for _, enabled := range []bool{false, true} {
		options := map[string]any{"bases": []any{map[string]any{
			"name": "custom-macos", "image": base.Image, "os_version": base.OSVersion,
		}}}
		if enabled {
			options["docker"] = map[string]any{}
		}
		c, _, err := macSettings(config.Provider{Type: "vz", Options: options}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if c.NeedsDockerEndpoint() != enabled || (c.Docker != nil) != enabled ||
			slices.Contains(c.BaseCapabilities("custom-macos"), "docker") != enabled {
			t.Fatalf("macOS-only Docker opt-in = %t, configuration = %+v", enabled, c)
		}
	}
}

func TestMacVMCountOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, options string
		want          [3]int
		invalid       bool
	}{
		{"omitted", "{}", [3]int{2, 4, 6}, false},
		{"partial", "{vm_counts: {linux: 1}}", [3]int{2, 1, 3}, false},
		{"shared limit", "{vm_counts: {total: 3}}", [3]int{2, 4, 3}, false},
		{"disable Linux", "{vm_counts: {linux: 0}}", [3]int{2, 0, 2}, false},
		{"drain", "{vm_counts: {total: 0}}", [3]int{2, 4, 0}, false},
		{"too many macOS", "{vm_counts: {macos: 3}}", [3]int{}, true},
		{"negative count", "{vm_counts: {linux: -1}}", [3]int{}, true},
		{"empty catalog", "{bases: []}", [3]int{}, true},
		{"empty slots", "{slots: []}", [3]int{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Parse([]byte("providers: {mac: {type: vz, config: " + tc.options + "}}\ntargets: [{jobdb: https://db/tenant, launch_services: [{name: mac, priority: 1}]}]"))
			if err != nil {
				t.Fatal(err)
			}
			c, _, err := macSettings(cfg.Providers["mac"], t.TempDir())
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid override accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := [3]int{*c.VMCounts.MacOS, *c.VMCounts.Linux, *c.VMCounts.Total}
			if got != tc.want {
				t.Fatalf("counts = %v, want %v", got, tc.want)
			}
			for _, slot := range c.Slots {
				if slot.CPU != pulsevm.DefaultCPU || slot.MemoryMiB != pulsevm.DefaultMemoryMiB || (tc.want[2] == 0 && slot.Count != 0) {
					t.Fatal("conventional size or drain lost", slot)
				}
			}
		})
	}
}

func TestVZLibraryConstructionAndCapabilities(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("VZ construction requires Apple Silicon")
	}
	for _, tc := range []struct {
		name             string
		macOSDocker, kvm bool
	}{
		{name: "defaults"},
		{name: "macOS Docker", macOSDocker: true},
		{name: "Linux KVM", kvm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := filepath.Join(t.TempDir(), "runner")
			// Answer the runner handshake without requiring a signed executable or VM.
			if err := os.WriteFile(runner, []byte(fmt.Sprintf(`#!/bin/sh
[ "$#" = 2 ] && [ "$1" = __pulse-vm ] || exit 1
case "$2" in
  probe) echo '"pulse-vz/1"' ;;
  features) echo '{"protocol":"pulse-vz/1","nested_virtualization":%t}' ;;
  *) exit 1 ;;
esac
`, tc.kvm)), 0700); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			options := map[string]any{"home": home, "runner": runner}
			p := config.Provider{Type: "vz", Options: map[string]any{"vz": options}}
			if tc.macOSDocker {
				p.Options["docker"] = map[string]any{}
				options["docker_endpoint"] = map[string]any{"address": "192.168.64.1", "port_min": 45100, "port_max": 45120}
			}
			cfg := &config.Config{Providers: map[string]config.Provider{"mac": p}}
			ps, closeAll, err := Build(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer closeAll()

			caps, err := ps["mac"].GetCapabilities(t.Context())
			want := append(pulsevm.DefaultBases(), pulsevm.DefaultLinuxBase())
			if err != nil || !caps.BasesComplete || len(caps.Bases) != len(want) {
				t.Fatal(caps, err)
			}
			for i, b := range caps.Bases {
				if b.Name != want[i].Name {
					t.Fatal("library catalog order changed", caps)
				}
				linux := b.Name == pulsevm.DefaultLinuxBase().Name
				docker := api.Allocation{Base: b.Name, Capabilities: []string{"docker"}}
				if got := providerutil.Compatible(caps, docker); got != (tc.macOSDocker || linux) {
					t.Fatal("unexpected Docker capability", b)
				}
				kvm := api.Allocation{Base: b.Name, Capabilities: []string{"kvm"}}
				if got := providerutil.Compatible(caps, kvm); got != (tc.kvm && linux) {
					t.Fatal("unexpected KVM capability", b)
				}
				for _, capability := range want[i].Capabilities {
					if !slices.Contains(b.Capabilities, capability) {
						t.Fatal("library capability omitted", b.Name, capability)
					}
				}
			}
			mac := ps["mac"].(*macProvider)
			if mac.slots != 8 || *mac.vmCounts.Total != 6 {
				t.Fatalf("unexpected default capacity: %d slots, %d total job VMs", mac.slots, *mac.vmCounts.Total)
			}
			page, err := mac.List(t.Context(), api.ListRequest{})
			if err != nil || len(page.Items) != 0 {
				t.Fatal(page, err)
			}
			launch := macLaunch(want[0].Name)
			results, details, _, err := mac.SubmitWithDiagnostics(t.Context(), []api.Launch{launch}, api.SubmitOptions{})
			if err != nil || len(results) != 1 || results[0].Status != api.NoCapacity || results[0].Reason == "" {
				t.Fatal("expected the unprepared pool to decline the launch", results, err)
			}
			if original := details[launch.LaunchID].OriginalResults; !reflect.DeepEqual(original, results) {
				t.Fatal("native diagnostics changed", original, results)
			}
			if entries, err := os.ReadDir(filepath.Join(home, "vms")); !errors.Is(err, os.ErrNotExist) && (err != nil || len(entries) != 0) {
				t.Fatal("provider startup created VMs", entries, err)
			}
			closeAll()
			_, reopened, err := Build(t.Context(), cfg)
			if err != nil {
				t.Fatal("owner lock not released", err)
			}
			reopened()
		})
	}
}

func TestMacDockerSettingsAndCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ENGINE_PASSWORD", "engine-secret")
	if err := os.WriteFile(filepath.Join(root, "host.pub"), []byte("engine-host-key"), 0600); err != nil {
		t.Fatal(err)
	}
	base := pulsevm.DefaultLinuxBase()
	p := config.Provider{Type: "vz", Options: map[string]any{
		"owner_id": "custom-pool",
		"bases":    []any{map[string]any{"name": "custom-linux", "os": "linux", "os_version": "24.04", "image": base.Image, "ssh_username": base.SSHUsername, "ssh_password_env": "ENGINE_PASSWORD", "capabilities": []string{"git"}}},
		"slots":    []any{map[string]any{"name": "linux", "count": 0, "base": "custom-linux", "docker_profile": "large"}},
		"docker":   map[string]any{"ready_timeout": "20m", "profiles": []any{map[string]any{"name": "large", "ssh_password_env": "ENGINE_PASSWORD", "ssh_host_key_file": "host.pub", "cpu": 4, "memory_mib": 8192, "disk_gib": 64, "engine_version": "29.1.3"}}},
		"vz":       map[string]any{"runner": "tools/runner", "home": "vm-home", "command_timeout": "2m", "ssh_timeout": "20s", "delivery_timeout": "1m", "docker_endpoint": map[string]any{"address": "192.168.64.1", "port_min": 45200, "port_max": 45210}},
	}}
	c, local, err := macSettings(p, root)
	if err != nil {
		t.Fatal(err)
	}
	if c.OwnerID != "custom-pool" {
		t.Fatalf("owner = %q, want custom-pool", c.OwnerID)
	}
	engine := c.Docker.Profiles[0]
	if len(c.Bases) != 1 || c.Bases[0].Name != "custom-linux" || c.Bases[0].OS != "linux" || c.Bases[0].SSHPassword != "engine-secret" || c.Slots[0].Count != 0 || c.Slots[0].DockerProfile != "large" {
		t.Fatal("custom base or drained Docker slot not preserved")
	}
	if c.Docker.ReadyTimeout != 20*time.Minute || engine.Name != "large" || engine.Image != pulsevm.DefaultDockerEngine().Image || engine.SSHPassword != "engine-secret" || string(engine.SSHHostKey) != "engine-host-key" || engine.CPU != 4 || engine.MemoryMiB != 8192 || engine.DiskGiB != 64 || engine.EngineVersion != "29.1.3" {
		t.Fatal("Docker profile settings not preserved")
	}
	if local.Runner != filepath.Join(root, "tools/runner") || local.Home != filepath.Join(root, "vm-home") ||
		local.CommandTimeout != 2*time.Minute || local.SSHTimeout != 20*time.Second || local.DeliveryTimeout != time.Minute ||
		local.DockerEndpoint != (pulsevm.DockerEndpoint{Address: "192.168.64.1", PortMin: 45200, PortMax: 45210}) {
		t.Fatal("local engine transport settings not preserved")
	}
	t.Setenv("ENGINE_PASSWORD", "")
	if _, _, err = macSettings(p, root); err == nil {
		t.Fatal("empty explicitly configured credential used default login")
	}
}

func TestVZRejectsIncompleteDockerConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		docker   any
		profile  string
		endpoint map[string]any
	}{
		{name: "profile without engine", profile: "standard"},
		{name: "missing endpoint", docker: map[string]any{}, profile: "standard"},
		{name: "unknown profile", docker: map[string]any{}, profile: "missing", endpoint: map[string]any{"address": "192.168.64.1", "port_min": 45100, "port_max": 45120}},
		{name: "empty profile list", docker: map[string]any{"profiles": []any{}}, profile: "standard", endpoint: map[string]any{"address": "192.168.64.1", "port_min": 45100, "port_max": 45120}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := config.Provider{Type: "vz", Options: map[string]any{
				"docker": tc.docker,
				"slots":  []any{map[string]any{"name": "mac", "count": 1, "base": pulsevm.DefaultBases()[0].Name, "docker_profile": tc.profile}},
				"vz":     map[string]any{"home": t.TempDir(), "docker_endpoint": tc.endpoint},
			}}
			_, closeAll, err := Build(t.Context(), &config.Config{Providers: map[string]config.Provider{"mac": p}})
			if err == nil {
				closeAll()
				t.Fatal("invalid Docker configuration reached a usable provider")
			}
			if !errors.Is(err, pulsevm.ErrConfig) {
				t.Fatal("expected configuration error", err)
			}
		})
	}
}

// This runtime can return authoritative results and errors together, or raw
// malformed results. The shim must not discard either observation.
type macDiagnosticRuntime struct {
	macFake
	submit func([]api.Launch) ([]api.Submission, error)
}

func (f *macDiagnosticRuntime) Submit(_ context.Context, ls []api.Launch, _ api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	f.calls = append(f.calls, ls...)
	out, err := f.submit(ls)
	return out, nil, err
}
func TestMacDiagnosticErrorsAndOriginalReasons(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprint(malformed), func(t *testing.T) {
			runtime := &macDiagnosticRuntime{submit: func(ls []api.Launch) ([]api.Submission, error) {
				results := []api.Submission{{LaunchID: ls[0].LaunchID, Status: api.NoCapacity, Reason: "occupied reservation: original native reason"}}
				if malformed {
					results = append(results, api.Submission{LaunchID: ls[0].LaunchID, Status: api.Unavailable, Reason: "second original reason"})
				}
				return results, errors.New("raw runtime error: " + ls[0].LaunchID)
			}}
			p := &macProvider{runtime: runtime}
			first, second := macLaunch("macos*"), macLaunch("macos*")
			second.LaunchID = "second"
			out, details, _, err := p.SubmitWithDiagnostics(t.Context(), []api.Launch{first, second}, api.SubmitOptions{})
			if err == nil || !strings.Contains(err.Error(), "raw runtime error: test") {
				t.Fatal(err)
			}
			d := details[first.LaunchID]
			if *d.Error != "raw runtime error: test" || d.OriginalResults[0].Reason != "occupied reservation: original native reason" {
				t.Fatal(d)
			}
			if malformed {
				if len(runtime.calls) != 1 || len(out) != 1 || out[0].Status != api.Unknown || len(d.OriginalResults) != 2 || d.NormalizationError == nil {
					t.Fatal(out, details, runtime.calls)
				}
			} else {
				if len(runtime.calls) != 2 || len(out) != 2 || out[0].Status != api.NoCapacity || !strings.Contains(err.Error(), "raw runtime error: second") || *details["second"].Error != "raw runtime error: second" {
					t.Fatal(out, details, err)
				}
			}
		})
	}
}
func TestMacReconciliationDiagnosticReason(t *testing.T) {
	runtime := &macFake{}
	p := &macProvider{runtime: runtime}
	p.activity.Lock()
	defer p.activity.Unlock()
	out, details, _, err := p.SubmitWithDiagnostics(t.Context(), []api.Launch{macLaunch("macos*")}, api.SubmitOptions{})
	if err != nil || len(runtime.calls) != 0 || len(details) != 0 || out[0].Status != api.NoCapacity || out[0].Reason != "macOS slots are being reconciled" {
		t.Fatal(out, details, err)
	}
}
