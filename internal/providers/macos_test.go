package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	tart "github.com/colony-2/pulse-tart"
	"github.com/colony-2/pulse/internal/config"
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
func TestMacSelectorsAndSafeFallback(t *testing.T) {
	catalog := []tart.Base{{Name: "macos-15-arm64"}, {Name: "macos-26-arm64"}}
	for _, tc := range []struct {
		name, base string
		outcomes   []api.Status
		invalid    bool
		want       api.Status
		calls      int
	}{
		{name: "middle wildcard", base: "macos-*arm64", want: api.Accepted, calls: 1},
		{name: "repeated stars", base: "macos**arm64", want: api.Accepted, calls: 1},
		{name: "aliases do not match", base: "darwin*", want: api.Unsupported, calls: 0},
		{name: "linux default", base: "ubuntu-26*arm64", want: api.Unsupported},
		{name: "next matching base", base: "macos*", outcomes: []api.Status{api.NoCapacity, api.Accepted}, want: api.Accepted, calls: 2},
		{name: "constraints", base: "macos*", outcomes: []api.Status{api.Unsupported, api.Accepted}, want: api.Accepted, calls: 2},
		{name: "unknown stops", base: "macos*", outcomes: []api.Status{api.Unknown, api.Accepted}, want: api.Unknown, calls: 1},
		{name: "malformed stops", base: "macos*", invalid: true, want: api.Unknown, calls: 1},
		{name: "unavailable stops", base: "macos*", outcomes: []api.Status{api.Unavailable}, want: api.Unavailable, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &macFake{outcomes: tc.outcomes, invalid: tc.invalid}
			p := &macProvider{runtime: f, bases: catalog}
			launch := macLaunch(tc.base)
			out, _ := p.Submit(t.Context(), []api.Launch{launch})
			if len(out) != 1 || out[0].Status != tc.want || len(f.calls) != tc.calls {
				t.Fatal(out, f.calls)
			}
			if launch.Base != tc.base {
				t.Fatal("selector changed")
			}
			for _, got := range f.calls {
				got.Base = tc.base
				if !reflect.DeepEqual(got, launch) {
					t.Fatal("assignment changed")
				}
			}
		})
	}
	f := &macFake{}
	p := &macProvider{runtime: f, bases: catalog, envUnsupported: true}
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
func TestOrchardLibraryConstructionAndFourSlots(t *testing.T) {
	var creates atomic.Int32
	specs := make(chan map[string]any, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/vms" {
			var spec map[string]any
			if json.NewDecoder(r.Body).Decode(&spec) != nil {
				t.Error("invalid VM request")
			}
			creates.Add(1)
			spec["uid"] = "uid-" + spec["name"].(string)
			specs <- spec
			_ = json.NewEncoder(w).Encode(spec)
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
	if creates.Load() != 0 {
		t.Fatal("construction/check created VMs")
	}
	p := providers["mac"].(*macProvider)
	caps, err := p.GetCapabilities(t.Context())
	if err != nil || !caps.BasesComplete || len(caps.Bases) != 1 || caps.Bases[0].Name != config.DefaultMacOSBase || len(caps.Bases[0].Capabilities) != 6 || creates.Load() != 0 {
		t.Fatal("library discovery lost support or provisioned VMs", caps, err)
	}
	page, err := p.List(t.Context(), api.ListRequest{})
	if err != nil || len(page.Items) != 0 || creates.Load() != 0 {
		t.Fatal("listing caused side effects", err)
	}
	if err = p.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		select {
		case spec := <-specs:
			if spec["cpu"] != float64(4) || spec["memory"] != float64(8192) || spec["image"] != config.DefaultMacOSImage || spec["suspendable"] != true {
				t.Fatal("wrong slot", spec["cpu"], spec["memory"])
			}
		case <-time.After(5 * time.Second):
			t.Fatal("four slots not prepared")
		}
	}
	closeAll()
	// Closing releases the durable lock; reopening does not duplicate templates.
	_, again, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatal("journal not released", err)
	}
	again()
	if creates.Load() != 4 {
		t.Fatal("reopen created extra slots")
	}
}
func TestMacSettingsPathsSecretsAndDrain(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MAC_TEST_PASSWORD", "private-password")
	t.Setenv("ORCHARD_TEST_TOKEN", "private-token")
	if err := os.WriteFile(filepath.Join(root, "ca.pem"), []byte("test-ca"), 0600); err != nil {
		t.Fatal(err)
	}
	p := config.Provider{Type: "orchard", Options: map[string]any{
		"state_dir": "state", "slots": []any{map[string]any{"name": "standard", "count": 0, "base": config.DefaultMacOSBase}},
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
