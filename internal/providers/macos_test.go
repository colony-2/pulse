package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	tart "github.com/colony-2/pulse-tart"
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

func (f *macFake) Submit(_ context.Context, ls []api.Launch) ([]api.Submission, error) {
	f.calls = append(f.calls, ls...)
	if f.invalid {
		return nil, errors.New("uncertain")
	}
	status := api.Accepted
	if len(f.outcomes) > 0 {
		status, f.outcomes = f.outcomes[0], f.outcomes[1:]
	}
	r := api.Submission{LaunchID: ls[0].LaunchID, Status: status}
	if status != api.Accepted {
		r.Reason = "test outcome"
	}
	return []api.Submission{r}, nil
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
			out, _ := p.Submit(t.Context(), []api.Launch{launch})
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
	out, err := p.Submit(t.Context(), []api.Launch{macLaunch("macos*")})
	if err != nil || out[0].Status != api.Unsupported || len(f.calls) != 0 {
		t.Fatal("environment ignored", out, err)
	}
	launch := macLaunch("macos*")
	if _, err = p.Submit(t.Context(), []api.Launch{launch, launch}); err == nil {
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
	out, err := p.Submit(t.Context(), []api.Launch{macLaunch("macos*")})
	if err != nil || out[0].Status != api.NoCapacity || len(f.calls) != 0 {
		t.Fatal("lease queued behind cold preparation", out, err)
	}
	p.close()
	if !f.closed.Load() {
		t.Fatal("runtime not closed")
	}
	if p.Maintain(t.Context()) != tart.ErrClosed {
		t.Fatal("closed runtime restarted")
	}
}
func TestOrchardLibraryConstructionUsesConventions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("construction/discovery mutated backend: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected mutation", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/controller/info":
			fmt.Fprint(w, `{"version":"test"}`)
		case "/v1/vms":
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`providers:
  mac:
    type: orchard
    config:
      state_dir: %q
      orchard: {url: %q, worker: mac, allow_http: true}
targets: [{jobdb: https://db/tenant, launch_services: [{name: mac, priority: 1}]}]
`, filepath.Join(t.TempDir(), "state"), server.URL)))
	if err != nil {
		t.Fatal(err)
	}
	providers, closeAll, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	p := providers["mac"].(*macProvider)
	want, err := tart.ResolveConfig(tart.Config{OwnerID: "test", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if p.slots != want.Capacity() || !reflect.DeepEqual(p.vmCounts, want.VMCounts) {
		t.Fatalf("incorrect conventional slot count: %d", p.slots)
	}
	caps, err := p.GetCapabilities(t.Context())
	if err != nil || !caps.BasesComplete || len(caps.Bases) != len(want.Bases) {
		t.Fatal("library discovery lost support or provisioned VMs", caps, err)
	}
	for i, base := range want.Bases {
		if caps.Bases[i].Name != base.Name {
			t.Fatal("library catalog order changed", caps)
		}
	}
	page, err := p.List(t.Context(), api.ListRequest{})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("listing caused side effects", err)
	}
	closeAll()
	// Closing releases the durable lock; reopening does not duplicate templates.
	_, again, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatal("journal not released", err)
	}
	again()
}
func TestMacSettingsPathsSecretsAndDrain(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MAC_TEST_PASSWORD", "private-password")
	t.Setenv("ORCHARD_TEST_TOKEN", "private-token")
	if err := os.WriteFile(filepath.Join(root, "ca.pem"), []byte("test-ca"), 0600); err != nil {
		t.Fatal(err)
	}
	p := config.Provider{Type: "orchard", Options: map[string]any{
		"state_dir": "state", "slots": []any{map[string]any{"name": "standard", "count": 0, "base": tart.DefaultBases()[0].Name}},
		"orchard": map[string]any{"url": "https://orchard.example", "worker": "mac", "username": "pulse", "token_env": "ORCHARD_TEST_TOKEN", "ca_cert_file": "ca.pem"},
	}}
	c, _, remote, err := macSettings("mac", p, root)
	if err != nil || c.StateDir != filepath.Join(root, "state") || c.Slots[0].Count != 0 || string(remote.CACert) != "test-ca" || string(remote.Token) != "private-token" {
		t.Fatal("wrong settings", err)
	}
	p.Options["orchard"].(map[string]any)["token_env"] = "EMPTY_PULSE_TEST_TOKEN"
	t.Setenv("EMPTY_PULSE_TEST_TOKEN", "")
	if _, _, _, err = macSettings("mac", p, root); err == nil {
		t.Fatal("missing token accepted")
	}
}

func (*macFake) GetCapabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{}, nil
}

func TestMacDefaultsComeFromLibrary(t *testing.T) {
	c, _, _, err := macSettings("mac", config.Provider{Type: "tart"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want, err := tart.ResolveConfig(tart.Config{OwnerID: c.OwnerID, StateDir: c.StateDir})
	if err != nil || !reflect.DeepEqual(c, want) {
		t.Fatal("Pulse replaced library conventions", err)
	}
	if c.NeedsDockerEndpoint() || c.BootstrapTimeout == 0 {
		t.Fatal("ordinary defaults need a worker endpoint or lost preparation timeout")
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
			cfg, err := config.Parse([]byte("providers: {mac: {type: tart, config: " + tc.options + "}}\ntargets: [{jobdb: https://db/tenant, launch_services: [{name: mac, priority: 1}]}]"))
			if err != nil {
				t.Fatal(err)
			}
			c, _, _, err := macSettings("mac", cfg.Providers["mac"], t.TempDir())
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
				if slot.CPU != tart.DefaultCPU || slot.MemoryMiB != tart.DefaultMemoryMiB || (tc.want[2] == 0 && slot.Count != 0) {
					t.Fatal("conventional size or drain lost", slot)
				}
			}
		})
	}
}

func TestOrchardDockerCapabilityDiscovery(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == "GET" && r.URL.Path == "/v1/controller/info" {
			fmt.Fprint(w, `{"version":"test"}`)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/vms" {
			fmt.Fprint(w, `[]`)
			return
		}
		t.Errorf("discovery caused backend operation: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected backend operation", http.StatusBadRequest)
	}))
	defer server.Close()
	linux := tart.DefaultLinuxBase().Name
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("docker=%v", enabled), func(t *testing.T) {
			docker, endpoint := "", ""
			if enabled {
				docker = "docker: {}"
				endpoint = "docker_endpoint: {address: 192.168.64.1, port_min: 45100, port_max: 45120}"
			}
			cfg, err := config.Parse([]byte(fmt.Sprintf(`providers:
  mac:
    type: orchard
    config:
      state_dir: %q
      %s
      orchard:
        url: %q
        worker: mac
        allow_http: true
        %s
targets: [{jobdb: https://db/tenant, launch_services: [{name: mac, priority: 1}]}]
`, filepath.Join(t.TempDir(), "state"), docker, server.URL, endpoint)))
			if err != nil {
				t.Fatal(err)
			}
			ps, closeAll, err := Build(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer closeAll()
			before := requests.Load()
			caps, err := ps["mac"].GetCapabilities(t.Context())
			if err != nil || !caps.BasesComplete {
				t.Fatal(caps, err)
			}
			want := append(tart.DefaultBases(), tart.DefaultLinuxBase())
			if len(caps.Bases) != len(want) {
				t.Fatal("provider catalog lost entries", caps)
			}
			for i, b := range caps.Bases {
				wantDocker := b.Name == linux || enabled
				if b.Name != want[i].Name || slices.Contains(b.Capabilities, "docker") != wantDocker {
					t.Fatal("provider capability discovery disagrees with slot configuration", caps)
				}
				for _, capability := range want[i].Capabilities {
					if !slices.Contains(b.Capabilities, capability) {
						t.Fatal("library capability omitted", b.Name, capability)
					}
				}
			}
			for _, base := range want {
				if providerutil.Compatible(caps, api.Allocation{Base: base.Name, Capabilities: []string{"docker"}}) != (base.Name == linux || enabled) {
					t.Fatal("scheduler compatibility does not follow discovery", base.Name, caps)
				}
			}
			if requests.Load() != before {
				t.Fatal("discovery contacted backend after connectivity check")
			}
		})
	}
}

func TestMacDockerSettingsAndCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ENGINE_PASSWORD", "engine-secret")
	if err := os.WriteFile(filepath.Join(root, "host.pub"), []byte("engine-host-key"), 0600); err != nil {
		t.Fatal(err)
	}
	base := tart.DefaultLinuxBase()
	p := config.Provider{Type: "tart", Options: map[string]any{
		"bases":  []any{map[string]any{"name": "custom-linux", "os": "linux", "os_version": "24.04", "image": base.Image, "ssh_username": base.SSHUsername, "ssh_password_env": "ENGINE_PASSWORD", "capabilities": []string{"git"}}},
		"slots":  []any{map[string]any{"name": "linux", "count": 0, "base": "custom-linux", "docker_profile": "large"}},
		"docker": map[string]any{"ready_timeout": "20m", "profiles": []any{map[string]any{"name": "large", "ssh_password_env": "ENGINE_PASSWORD", "ssh_host_key_file": "host.pub", "cpu": 4, "memory_mib": 8192, "disk_gib": 64, "engine_version": "29.1.3"}}},
		"tart":   map[string]any{"python_binary": "tools/python3", "docker_endpoint": map[string]any{"address": "192.168.64.1", "port_min": 45200, "port_max": 45210}},
	}}
	c, local, _, err := macSettings("mac", p, root)
	if err != nil {
		t.Fatal(err)
	}
	engine := c.Docker.Profiles[0]
	if len(c.Bases) != 1 || c.Bases[0].Name != "custom-linux" || c.Bases[0].OS != "linux" || c.Bases[0].SSHPassword != "engine-secret" || c.Slots[0].Count != 0 || c.Slots[0].DockerProfile != "large" {
		t.Fatal("custom base or drained Docker slot not preserved")
	}
	if c.Docker.ReadyTimeout != 20*time.Minute || engine.Name != "large" || engine.Image != tart.DefaultDockerEngine().Image || engine.SSHPassword != "engine-secret" || string(engine.SSHHostKey) != "engine-host-key" || engine.CPU != 4 || engine.MemoryMiB != 8192 || engine.DiskGiB != 64 || engine.EngineVersion != "29.1.3" {
		t.Fatal("Docker profile settings not preserved")
	}
	if local.PythonBinary != filepath.Join(root, "tools/python3") || local.DockerEndpoint != (tart.DockerEndpoint{Address: "192.168.64.1", PortMin: 45200, PortMax: 45210}) {
		t.Fatal("local engine transport settings not preserved")
	}
	t.Setenv("ENGINE_PASSWORD", "")
	if _, _, _, err = macSettings("mac", p, root); err == nil {
		t.Fatal("empty explicitly configured credential used default login")
	}
}

func TestOrchardRejectsIncompleteDockerConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		docker   any
		profile  string
		endpoint map[string]any
	}{
		{name: "profile without engine", profile: "standard"},
		{name: "missing worker endpoint", docker: map[string]any{}, profile: "standard"},
		{name: "unknown profile", docker: map[string]any{}, profile: "missing", endpoint: map[string]any{"address": "192.168.64.1", "port_min": 45100, "port_max": 45120}},
		{name: "empty profile list", docker: map[string]any{"profiles": []any{}}, profile: "standard", endpoint: map[string]any{"address": "192.168.64.1", "port_min": 45100, "port_max": 45120}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := config.Provider{Type: "orchard", Options: map[string]any{
				"state_dir": filepath.Join(t.TempDir(), "state"),
				"docker":    tc.docker,
				"slots":     []any{map[string]any{"name": "mac", "count": 1, "base": tart.DefaultBases()[0].Name, "docker_profile": tc.profile}},
				"orchard":   map[string]any{"url": "https://orchard.example", "worker": "mac", "docker_endpoint": tc.endpoint},
			}}
			c, _, remote, err := macSettings("mac", p, t.TempDir())
			if err != nil {
				return // Library validation runs while resolving the effective settings.
			}
			r, err := tart.NewOrchard(c, remote)
			if err == nil {
				r.Close()
				t.Fatal("invalid Docker configuration reached a usable provider")
			}
		})
	}
}
