package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/jobdb/pkg/jobdb"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/placement"
	"github.com/colony-2/pulse/internal/scheduler"
)

type diagnosticLease struct {
	testLease
	request jobdb.RescheduleExecutionRequest
	err     error
	calls   int
}

func (l *diagnosticLease) Reschedule(_ context.Context, r jobdb.RescheduleExecutionRequest) error {
	l.request = r
	l.calls++
	return l.err
}

type diagnosticProvider struct {
	accept
	reason       string
	err          error
	discoveryErr error
	status       api.Status
}

func (p *diagnosticProvider) GetCapabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{}, p.discoveryErr
}
func (p *diagnosticProvider) Submit(ctx context.Context, ls []api.Launch) ([]api.Submission, error) {
	out, _ := p.accept.Submit(ctx, ls)
	for i := range out {
		out[i].Status = p.status
		out[i].Reason = p.reason
	}
	return out, p.err
}

type launchDiagnostic struct {
	Message     string                `json:"msg"`
	Level       string                `json:"level"`
	Instance    string                `json:"instance"`
	Tenant      string                `json:"tenant"`
	Job         string                `json:"job"`
	LaunchID    string                `json:"launch_id"`
	Service     string                `json:"service"`
	Status      api.Status            `json:"status"`
	Reason      string                `json:"reason"`
	Requested   placement.Requested   `json:"requested"`
	Preparation placement.Preparation `json:"preparation"`
	Attempts    placement.Attempts    `json:"attempts"`
	Admission   string                `json:"execution_admission"`
	Retry       placement.Retry       `json:"retry"`
}

func diagnosticFixture(t *testing.T) (*Controller, *diagnosticProvider, *diagnosticLease, *bytes.Buffer) {
	t.Helper()
	p := &diagnosticProvider{reason: "macOS slots are being reconciled", status: api.NoCapacity, err: errors.New("native admission: worker unavailable\nraw \"response\": " + strings.Repeat("x", 5000))}
	lease := &diagnosticLease{}
	cfg := &config.Config{Call: time.Second, Batch: time.Second, Cool: time.Minute, BatchSize: 1, PerTenant: 1, MaxPages: 1, Allocation: api.Allocation{Base: "ubuntu-26*", CPUMillis: 2000, MemoryBytes: 2 << 30, Capabilities: []string{"git"}}, Providers: map[string]config.Provider{"tart": {Type: "tart"}}}
	cfg.Targets = []config.Target{{Instance: "db", Tenant: "tenant", JobDB: "https://db/tenant", Services: []config.Service{{Name: "tart", Priority: 1}}}}
	listed := c2j.Job{Tenant: "tenant", ID: "job", Execution: &c2j.View{Status: "not_loaded"}}
	base, memory := "macos*arm64", "8Gi"
	d, err := execution.Initial(nil, true, execution.Requirements{Base: &base, Resources: execution.Resources{Memory: &memory}, Capabilities: []string{"docker"}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := execution.PayloadWithDemand(nil, d.Demand)
	if err != nil {
		t.Fatal(err)
	}
	view := execution.Inspect(nil, payload)
	var buf bytes.Buffer
	c := &Controller{Config: cfg, Providers: map[string]api.Provider{"tart": p}, Scheduler: scheduler.New(time.Minute, time.Second), Log: slog.New(slog.NewJSONHandler(&buf, nil))}
	// Ensure the two deadline gates differ. Eligibility must use the later gate,
	// not the time the controller happens to write its log.
	localNow := time.Now().UTC().Add(24 * time.Hour)
	c.Scheduler.Now = func() time.Time { return localNow }
	c.Lister = listFunc(func(context.Context, string, string, []c2j.Route) (c2j.Page, error) {
		return c2j.Page{Jobs: []c2j.Job{listed}}, nil
	})
	c.Claimer = claimFunc(func(context.Context, string, c2j.Job, string, time.Duration, []c2j.Route) (*c2j.Claimed, error) {
		job := listed
		job.Execution = &view
		return &c2j.Claimed{Job: job, Lease: lease, Encoded: []byte("exported-lease-sentinel")}, nil
	})
	return c, p, lease, &buf
}
func decodeLaunch(t *testing.T, buf *bytes.Buffer) launchDiagnostic {
	t.Helper()
	var d launchDiagnostic
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &d); err != nil {
		t.Fatal(err, buf.String())
	}
	return d
}
func factTime(t *testing.T, f placement.Fact) time.Time {
	t.Helper()
	s, ok := f.Value.(string)
	if f.State != "known" || !ok {
		t.Fatalf("expected known timestamp: %+v", f)
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestNoCapacityCorrelatedDiagnosticAndDeadlines(t *testing.T) {
	c, p, l, buf := diagnosticFixture(t)
	p.discoveryErr = errors.New("discovery original response")
	if err := c.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := decodeLaunch(t, buf)
	t.Logf("correlated diagnostic: %s", strings.TrimSpace(buf.String()))
	if d.Message != "launch result" || d.Level != "INFO" || d.Instance != "db" || d.Tenant != "tenant" || d.Job != "job" || d.Service != "tart" || d.LaunchID == "" || d.Status != api.NoCapacity || d.Admission != "not_accepted" {
		t.Fatal(d)
	}
	if len(p.requests) != 1 || d.LaunchID != p.requests[0].LaunchID || p.requests[0].Base != "macos*arm64" || p.requests[0].MemoryBytes != 8<<30 {
		t.Fatal("wrong leased demand", p.requests)
	}
	if d.Requested.Base.Value != p.requests[0].Base || d.Requested.CPUMillis.Value != float64(2000) || d.Requested.MemoryBytes.Value != float64(8<<30) || !reflect.DeepEqual(d.Requested.Capabilities.Value, []any{"git", "docker"}) {
		t.Fatal(d.Requested)
	}
	if d.Requested.ScratchBytes.State != "unspecified" || d.Requested.ScratchBytes.Value != nil || d.Requested.Platform.State != "unknown" {
		t.Fatal(d.Requested)
	}
	if len(d.Attempts) != 1 {
		t.Fatal(d.Attempts)
	}
	a := d.Attempts[0]
	if a.ProviderPreparation.Observation != "unknown" || a.ProviderPreparation.Scope != "provider" || a.ProviderPreparation.Error != nil {
		t.Fatal("invented provider preparation observation", a.ProviderPreparation)
	}
	if a.Service != "tart" || a.ProviderType != "tart" || !a.SubmitCalled || *a.SubmissionError != p.err.Error() || *a.DiscoveryError != p.discoveryErr.Error() || a.SubmissionErrorScope != "batch" || a.OriginalResults[0].Reason != p.reason || d.Reason != p.reason {
		t.Fatal(a)
	}
	for _, f := range []placement.Fact{a.Capacity.Occupied, a.Capacity.AvailableWorkers, a.Capacity.ResourceLimits, a.Capacity.AdmissionConstraint} {
		if f.State != "unknown" || f.Value != nil {
			t.Fatal(f)
		}
	}
	if l.calls != 1 || l.request.WaitUntil == nil || !factTime(t, d.Retry.Release.RequestedUntil).Equal(*l.request.WaitUntil) || !factTime(t, d.Retry.Release.AcknowledgedUntil).Equal(*l.request.WaitUntil) {
		t.Fatal("deadline not actual Reschedule", d.Retry)
	}
	cooldowns := c.Scheduler.Cooldowns()
	if len(cooldowns) != 1 || !factTime(t, d.Retry.LocalEligibleAt).Equal(cooldowns[0].EligibleAt) || !factTime(t, d.Retry.NextEligibleAt).Equal(cooldowns[0].EligibleAt) || d.Retry.Cooldown.Value != "1m0s" {
		t.Fatal(d.Retry, cooldowns)
	}
	if strings.Contains(buf.String(), "exported-lease-sentinel") || strings.Contains(buf.String(), "environment") {
		t.Fatal("request leaked")
	}
	buf.Reset()
	if err := c.Once(t.Context()); err != nil || len(p.requests) != 1 || buf.Len() != 0 {
		t.Fatal("backoff changed", err, p.requests, buf.String())
	}
}
func TestLaunchDiagnosticReleaseFailure(t *testing.T) {
	c, _, l, buf := diagnosticFixture(t)
	l.err = errors.New("reschedule failed")
	if err := c.Once(t.Context()); err == nil {
		t.Fatal("lost cleanup pass error")
	}
	d := decodeLaunch(t, buf)
	if d.Retry.Release.Outcome != "failed" || d.Retry.Release.Error == nil || d.Retry.Release.AcknowledgedUntil.State != "unknown" || d.Retry.NextEligibleAt.State != "unknown" || d.Retry.NextEligibleAt.Value != nil || !factTime(t, d.Retry.Release.RequestedUntil).Equal(*l.request.WaitUntil) {
		t.Fatal(d.Retry)
	}
}
func TestLaunchDiagnosticAcceptedAndUncertain(t *testing.T) {
	for _, status := range []api.Status{api.Accepted, api.Unknown} {
		t.Run(string(status), func(t *testing.T) {
			c, p, l, buf := diagnosticFixture(t)
			p.status = status
			if status == api.Accepted {
				p.reason = ""
			}
			if err := c.Once(t.Context()); err != nil {
				t.Fatal(err)
			}
			d := decodeLaunch(t, buf)
			admission := "accepted"
			if status == api.Unknown {
				admission = "uncertain"
			}
			if d.Status != status || d.Admission != admission || l.calls != 0 || len(c.Scheduler.Cooldowns()) != 0 || d.Retry.Cooldown.State != "unspecified" || d.Retry.NextEligibleAt.Value != nil || *d.Attempts[0].SubmissionError != p.err.Error() {
				t.Fatal(d, l.calls)
			}
		})
	}
}
func TestLaunchDiagnosticPreparationAndText(t *testing.T) {
	c, p, l, buf := diagnosticFixture(t)
	c.Claimer = claimFunc(func(context.Context, string, c2j.Job, string, time.Duration, []c2j.Route) (*c2j.Claimed, error) {
		return &c2j.Claimed{Lease: l, Job: c2j.Job{ID: "job", Tenant: "tenant"}}, errors.New("original claim preparation error")
	})
	if err := c.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := decodeLaunch(t, buf)
	if *d.Preparation.Error != "original claim preparation error" || d.Preparation.Stage != "claim" || d.Requested.MemoryBytes.State != "unknown" || len(d.Attempts) != 0 || len(p.requests) != 0 || l.calls != 1 {
		t.Fatal(d)
	}
	c, p, _, buf = diagnosticFixture(t)
	c.Log = slog.New(slog.NewTextHandler(buf, nil))
	if err := c.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"launch result", "instance=db", "tenant=tenant", "job=job", "launch_id=", "service=tart", "macOS slots are being reconciled", "cpu_millis", "next_eligible_at", `raw \\\"response\\\"`} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("text missing %q: %s", want, buf.String())
		}
	}
	if !strings.Contains(buf.String(), strings.Repeat("x", 5000)) {
		t.Fatal("raw error truncated")
	}
	_ = p
}

func TestLaunchDiagnosticJobDBDeadlineCanBeLater(t *testing.T) {
	c, _, l, buf := diagnosticFixture(t)
	localNow := time.Now().UTC().Add(-24 * time.Hour)
	c.Scheduler.Now = func() time.Time { return localNow }
	if err := c.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := decodeLaunch(t, buf)
	if !factTime(t, d.Retry.NextEligibleAt).Equal(*l.request.WaitUntil) || !factTime(t, d.Retry.LocalEligibleAt).Before(*l.request.WaitUntil) {
		t.Fatal(d.Retry)
	}
}
func TestLaunchDiagnosticAllocationSurvivesLeaseExportFailure(t *testing.T) {
	c, p, l, buf := diagnosticFixture(t)
	claimer := c.Claimer
	c.Claimer = claimFunc(func(ctx context.Context, uri string, j c2j.Job, id string, d time.Duration, routes []c2j.Route) (*c2j.Claimed, error) {
		claimed, err := claimer.Claim(ctx, uri, j, id, d, routes)
		claimed.Encoded = nil
		return claimed, err
	})
	if err := c.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := decodeLaunch(t, buf)
	if d.Preparation.Stage != "exported_lease" || *d.Preparation.Error != "empty exported lease" || d.Requested.MemoryBytes.Value != float64(8<<30) || len(p.requests) != 0 || l.calls != 1 {
		t.Fatal(d)
	}
}
