package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/placement"
	"github.com/colony-2/pulse/internal/providers/adapter"
	"github.com/colony-2/pulse/internal/providers/remote"
	"github.com/colony-2/pulse/pkg/compute"
	"github.com/colony-2/pulse/pkg/providerplugin"
)

// A real plugin subprocess returns these strings through the SDK's gRPC server.
// The multiline method error deliberately exceeds the per-item reason limit.
const transportReason = "slots occupied; all reservations admitted"

func transportRawError() string {
	return "native quota lookup failed: reservation ledger unavailable\nraw \"response\": " + strings.Repeat("x", 5000)
}

type transportPluginConfig struct {
	Status     string `yaml:"status"`
	EmptyError bool   `yaml:"empty_error"`
	ErrorBytes int    `yaml:"error_bytes"`
}
type transportPluginProvider struct{ config transportPluginConfig }

type transportRecorder struct {
	compute.Provider
	err      error
	calls    int
	launches []compute.Launch
}

func (p *transportRecorder) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	p.calls++
	p.launches = append(p.launches, ls...)
	out, err := p.Provider.Submit(ctx, ls)
	p.err = err
	return out, err
}

// Forward optional observations so the valid/accepted/error controls exercise
// the same adapter path as production while recording calls independently.
func (p *transportRecorder) SubmitWithDiagnostics(ctx context.Context, ls []compute.Launch) ([]compute.Submission, compute.SubmissionDiagnostics, error) {
	p.calls++
	p.launches = append(p.launches, ls...)
	var out []compute.Submission
	var d compute.SubmissionDiagnostics
	var err error
	if detailed, ok := p.Provider.(compute.DiagnosticSubmitter); ok {
		out, d, err = detailed.SubmitWithDiagnostics(ctx, ls)
	} else {
		out, err = p.Provider.Submit(ctx, ls)
	}
	p.err = err
	return out, d, err
}

func (p transportPluginProvider) Submit(_ context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	out := make([]compute.Submission, len(ls))
	for i, l := range ls {
		reason := transportReason
		if p.config.Status == string(compute.Accepted) {
			reason = ""
		}
		out[i] = compute.Submission{LaunchID: l.LaunchID, Status: compute.Status(p.config.Status), Reason: reason}
	}
	raw := transportRawError()
	if p.config.ErrorBytes != 0 {
		raw += strings.Repeat("x", p.config.ErrorBytes)
	}
	if p.config.EmptyError {
		raw = ""
	}
	return out, errors.New(raw)
}
func (transportPluginProvider) List(context.Context, compute.ListRequest) (compute.ListResponse, error) {
	panic("placement diagnostics must not list instances")
}
func TestPlacementDiagnosticPluginProcess(t *testing.T) {
	if os.Getenv("PULSE_PLUGIN_COOKIE") == "" {
		return
	}
	providerplugin.Serve(func(_ context.Context, init providerplugin.Init) (compute.Provider, error) {
		var cfg transportPluginConfig
		if err := providerplugin.DecodeConfig(init.ConfigYAML, &cfg); err != nil {
			return nil, err
		}
		return transportPluginProvider{cfg}, nil
	})
	os.Exit(0)
}

func transportDiagnosticFixture(t *testing.T, native compute.Provider, service string) (*Controller, *diagnosticLease, *bytes.Buffer) {
	t.Helper()
	c, _, lease, buf := diagnosticFixture(t)
	c.Providers = map[string]api.Provider{service: &adapter.Provider{Native: native}}
	c.Config.Providers = map[string]config.Provider{service: {Type: service}}
	c.Config.Targets[0].Services[0].Name = service
	c.Config.Allocation.Base = "ubuntu-26.04-arm64"
	c.Config.Allocation.Capabilities = nil
	// Use the configured allocation when leased demand is unavailable, without
	// changing the fixture's actual lease or Reschedule observer.
	claimer := c.Claimer
	c.Claimer = claimFunc(func(ctx context.Context, uri string, j c2j.Job, id string, duration time.Duration, routes []c2j.Route) (*c2j.Claimed, error) {
		claimed, err := claimer.Claim(ctx, uri, j, id, duration, routes)
		if err == nil {
			claimed.Job.Execution = &c2j.View{Status: "unresolved"}
		}
		return claimed, err
	})
	return c, lease, buf
}

func checkTransportDiagnostic(t *testing.T, c *Controller, lease *diagnosticLease, buf *bytes.Buffer, service, raw string, status api.Status) {
	t.Helper()
	if err := c.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := decodeLaunch(t, buf)
	t.Logf("correlated transport diagnostic: %s", strings.TrimSpace(buf.String()))
	if d.Message != "launch result" || d.Level != "INFO" || d.Instance != "db" || d.Tenant != "tenant" || d.Job != "job" || d.LaunchID == "" || d.Service != service || d.Status != status {
		t.Fatal(d)
	}
	if d.Requested.Base.Value != "ubuntu-26.04-arm64" || d.Requested.Platform.Value != "linux/arm64" || d.Requested.CPUMillis.Value != float64(2000) || d.Requested.MemoryBytes.Value != float64(2<<30) || d.Requested.ScratchBytes.State != "unspecified" {
		t.Fatal(d.Requested)
	}
	if len(d.Attempts) != 1 {
		t.Fatal(d.Attempts)
	}
	a := d.Attempts[0]
	if a.Service != service || a.ProviderType != service || a.SubmissionError == nil || *a.SubmissionError != raw || a.SubmissionErrorScope != "batch" || len(a.OriginalResults) != 1 || a.OriginalResults[0].LaunchID != d.LaunchID || a.OriginalResults[0].Status != status || a.OriginalResults[0].Reason != d.Reason {
		t.Fatal(a)
	}
	for _, f := range []placement.Fact{a.Capacity.Occupied, a.Capacity.AvailableWorkers, a.Capacity.ResourceLimits, a.Capacity.AdmissionConstraint} {
		if f.State != "unknown" || f.Value != nil {
			t.Fatal(f)
		}
	}
	if status == api.Accepted || status == api.Unknown {
		admission := "accepted"
		if status == api.Unknown {
			admission = "uncertain"
		}
		if d.Admission != admission || lease.calls != 0 || len(c.Scheduler.Cooldowns()) != 0 || d.Retry.Cooldown.State != "unspecified" || d.Retry.NextEligibleAt.Value != nil {
			t.Fatal(d.Retry, lease.calls, d.Admission)
		}
	} else {
		if d.Admission != "not_accepted" || lease.calls != 1 || lease.request.WaitUntil == nil || !factTime(t, d.Retry.Release.RequestedUntil).Equal(*lease.request.WaitUntil) || !factTime(t, d.Retry.Release.AcknowledgedUntil).Equal(*lease.request.WaitUntil) {
			t.Fatal(d.Retry, lease.calls)
		}
		cooldowns := c.Scheduler.Cooldowns()
		if len(cooldowns) != 1 || !factTime(t, d.Retry.LocalEligibleAt).Equal(cooldowns[0].EligibleAt) || !factTime(t, d.Retry.NextEligibleAt).Equal(cooldowns[0].EligibleAt) || d.Retry.Cooldown.Value != "1m0s" {
			t.Fatal(d.Retry, cooldowns)
		}
	}
	if strings.Contains(buf.String(), "exported-lease-sentinel") || strings.Contains(buf.String(), "C2J_EXECUTION_") {
		t.Fatal("launch payload leaked")
	}
}

func TestRemoteErrorBodyReachesLaunchDiagnostic(t *testing.T) {
	const body = "{\"code\":\"resource_limit\",\"message\":\"requested memory exceeds tenant quota\"}\noriginal \"quota\" response"
	for _, code := range []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/v1/submit" {
					t.Error(r.Method, r.URL.Path)
				}
				w.WriteHeader(code)
				w.Write([]byte(body))
			}))
			defer s.Close()
			native, err := remote.New(s.URL, s.Client(), nil, true)
			if err != nil {
				t.Fatal(err)
			}
			c, lease, buf := transportDiagnosticFixture(t, native, "remote")
			status := api.Rejected
			if code == http.StatusServiceUnavailable {
				status = api.Unknown
			}
			checkTransportDiagnostic(t, c, lease, buf, "remote", "provider returned HTTP "+fmt.Sprint(code)+": "+body, status)
			if calls != 1 {
				t.Fatal("submission retried", calls)
			}
		})
	}
}

func TestPluginMethodErrorReachesLaunchDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		status api.Status
		empty  bool
	}{{api.NoCapacity, false}, {api.Accepted, false}, {api.Unknown, false}, {api.NoCapacity, true}} {
		name := string(tc.status)
		if tc.empty {
			name += "_empty_error"
		}
		t.Run(name, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(map[string]any{"status": string(tc.status), "empty_error": tc.empty})
			if err != nil {
				t.Fatal(err)
			}
			native, err := providerplugin.Start(t.Context(), providerplugin.ProcessConfig{Command: executable, Args: []string{"-test.run=^TestPlacementDiagnosticPluginProcess$"}, Name: "diagnostic-fixture", BaseDir: t.TempDir(), ConfigYAML: data, StartTimeout: 15 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			recorded := &transportRecorder{Provider: native}
			c, lease, buf := transportDiagnosticFixture(t, recorded, "plugin")
			raw := transportRawError()
			if tc.empty {
				raw = ""
			}
			checkTransportDiagnostic(t, c, lease, buf, "plugin", raw, tc.status)
			if recorded.calls != 1 || !errors.Is(recorded.err, providerplugin.ErrProtocol) || recorded.err.Error() != raw || len(recorded.launches) != 1 {
				t.Fatal("method error classification or call count changed", recorded.err, recorded.calls)
			}
			d := decodeLaunch(t, buf)
			if d.LaunchID != recorded.launches[0].LaunchID || d.Requested.CPUMillis.Value != float64(recorded.launches[0].CPUMillis) || d.Requested.MemoryBytes.Value != float64(recorded.launches[0].MemoryBytes) {
				t.Fatal("diagnostic differs from transported allocation", d.Requested, recorded.launches[0].Request)
			}
			wantReason := transportReason
			if tc.status == api.Accepted {
				wantReason = ""
			}
			if d.Reason != wantReason {
				t.Fatal("provider Reason changed", d.Reason)
			}
			health, err := json.Marshal(native.Health())
			if err != nil || strings.Contains(string(health), "native quota") {
				t.Fatal("raw method error entered public health", string(health), err)
			}
			if tc.status == api.NoCapacity {
				buf.Reset()
				if err := c.Once(t.Context()); err != nil || lease.calls != 1 || buf.Len() != 0 || recorded.calls != 1 {
					t.Fatal("cooldown changed", err, lease.calls, buf.String())
				}
			}
		})
	}
}

func TestPluginOversizedMethodErrorKeepsAcceptance(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const limit = providerplugin.MaxConfigBytes + 1024
	data, err := json.Marshal(map[string]any{"status": "accepted", "error_bytes": 2 * limit})
	if err != nil {
		t.Fatal(err)
	}
	native, err := providerplugin.Start(t.Context(), providerplugin.ProcessConfig{Command: executable, Args: []string{"-test.run=^TestPlacementDiagnosticPluginProcess$"}, Name: "diagnostic-fixture", BaseDir: t.TempDir(), ConfigYAML: data, MaxMessageBytes: limit, StartTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	recorded := &transportRecorder{Provider: native}
	c, lease, buf := transportDiagnosticFixture(t, recorded, "plugin")
	if err := c.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := decodeLaunch(t, buf)
	if d.Status != api.Accepted || d.Admission != "accepted" || d.Reason != "" || lease.calls != 0 || recorded.calls != 1 || len(c.Scheduler.Cooldowns()) != 0 || d.Retry.NextEligibleAt.Value != nil {
		t.Fatal("diagnostic size changed acceptance", d.Status, d.Admission, lease.calls)
	}
	if len(d.Attempts) != 1 || d.Attempts[0].SubmissionError == nil || !errors.Is(recorded.err, providerplugin.ErrProtocol) {
		t.Fatal("lost oversized method diagnostic")
	}
	text := *d.Attempts[0].SubmissionError
	marker := strings.LastIndex(text, "\n[truncated after ")
	if marker < len(transportRawError()) || text != recorded.err.Error() || !strings.HasPrefix(text, transportRawError()) || !strings.HasSuffix(text, "bytes; provider error exceeds negotiated response limit]") || len(text) >= limit {
		t.Fatal("missing explicit capture bound", len(text), marker)
	}
	var kept int
	if _, err := fmt.Sscanf(text[marker:], "\n[truncated after %d bytes; provider error exceeds negotiated response limit]", &kept); err != nil || kept != marker {
		t.Fatal("inaccurate retained-byte count", kept, marker, err)
	}
}
