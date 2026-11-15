package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/jobdb/pkg/jobdb"
	api "github.com/colony-2/pulse-provider-api"
	df "github.com/colony-2/pulse-tart/tests/integration/diagnosticfixture"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/controller"
	"github.com/colony-2/pulse/internal/placement"
	"github.com/colony-2/pulse/internal/scheduler"
)

// These tests use the pinned runtime's loopback Orchard/SSH fixture as test
// support. No backend result is synthesized between runtime and operator log.
type tartDiagnosticRecorder struct {
	macRuntime
	fixture         *df.TartDiagnosticFixture
	launches        []api.Launch
	results         []api.Submission
	submitError     error
	discoveries     int
	discoveryProbed bool
}

func (r *tartDiagnosticRecorder) GetCapabilities(ctx context.Context) (api.Capabilities, error) {
	before := r.fixture.Snapshot()
	out, err := r.macRuntime.GetCapabilities(ctx)
	r.discoveryProbed = r.discoveryProbed || !reflect.DeepEqual(before, r.fixture.Snapshot())
	r.discoveries++
	return out, err
}
func (r *tartDiagnosticRecorder) Submit(ctx context.Context, ls []api.Launch) ([]api.Submission, error) {
	r.launches = append(r.launches, ls...)
	out, err := r.macRuntime.Submit(ctx, ls)
	r.results = append(r.results, out...)
	r.submitError = err
	return out, err
}

// Controller.Once normally starts asynchronous reconciliation. Explicit fixture
// setup owns reconciliation here; placement still uses the real macOS shim.
type tartDiagnosticHost struct {
	*macProvider
	maintenanceError error
}

func (p *tartDiagnosticHost) Maintain(context.Context) error { return p.maintenanceError }

type tartDiagnosticLease struct {
	jobdb.ExecutionLease
	requests []jobdb.RescheduleExecutionRequest
}

func (*tartDiagnosticLease) Route() jobdb.Route                   { return jobdb.Route{JobType: "recipe"} }
func (*tartDiagnosticLease) ExecutionState() jobdb.ExecutionState { return jobdb.ExecutionState{} }
func (l *tartDiagnosticLease) Reschedule(_ context.Context, r jobdb.RescheduleExecutionRequest) error {
	l.requests = append(l.requests, r)
	return nil
}

type tartDiagnosticList struct{ job c2j.Job }

func (l tartDiagnosticList) List(context.Context, string, string, []c2j.Route) (c2j.Page, error) {
	return c2j.Page{Jobs: []c2j.Job{l.job}}, nil
}

type tartDiagnosticClaimer struct {
	job   c2j.Job
	lease *tartDiagnosticLease
}

func (c tartDiagnosticClaimer) Claim(context.Context, string, c2j.Job, string, time.Duration, []c2j.Route) (*c2j.Claimed, error) {
	return &c2j.Claimed{Job: c.job, Lease: c.lease, Encoded: []byte("FIXTURE-LEASE-DO-NOT-CAPTURE")}, nil
}

type tartDiagnosticFallback struct{ launches []api.Launch }

func (*tartDiagnosticFallback) GetCapabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{}, nil
}
func (*tartDiagnosticFallback) List(context.Context, api.ListRequest) (api.ListResponse, error) {
	panic("placement diagnostic listed instances")
}
func (p *tartDiagnosticFallback) Submit(_ context.Context, ls []api.Launch) ([]api.Submission, error) {
	p.launches = append(p.launches, ls...)
	out := make([]api.Submission, len(ls))
	for i, l := range ls {
		out[i] = api.Submission{LaunchID: l.LaunchID, Status: api.Accepted}
	}
	return out, nil
}

type tartOperatorRecord struct {
	Message   string              `json:"msg"`
	Level     string              `json:"level"`
	Instance  string              `json:"instance"`
	Tenant    string              `json:"tenant"`
	Job       string              `json:"job"`
	LaunchID  string              `json:"launch_id"`
	Service   string              `json:"service"`
	Status    api.Status          `json:"status"`
	Reason    string              `json:"reason"`
	Requested placement.Requested `json:"requested"`
	Admission string              `json:"execution_admission"`
	Attempts  placement.Attempts  `json:"attempts"`
	Retry     placement.Retry     `json:"retry"`
}
type tartDiagnosticHarness struct {
	controller *controller.Controller
	runtime    *tartDiagnosticRecorder
	host       *tartDiagnosticHost
	lease      *tartDiagnosticLease
	fallback   *tartDiagnosticFallback
	log        bytes.Buffer
}

func newTartDiagnosticHarness(t *testing.T, f *df.TartDiagnosticFixture, fallback bool) *tartDiagnosticHarness {
	t.Helper()
	h := &tartDiagnosticHarness{lease: &tartDiagnosticLease{}, fallback: &tartDiagnosticFallback{}}
	h.runtime = &tartDiagnosticRecorder{macRuntime: f.Runtime, fixture: f}
	h.host = &tartDiagnosticHost{macProvider: &macProvider{runtime: h.runtime}}
	sample := f.Launch("allocation-example")
	base, cpu, memory := sample.Base, "4", "8Gi"
	if f.Config.Docker != nil {
		memory = "7680Mi"
	}
	initial, err := execution.Initial(nil, true, execution.Requirements{Base: &base, Resources: execution.Resources{CPU: &cpu, Memory: &memory}, Capabilities: sample.Capabilities})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := execution.PayloadWithDemand(nil, initial.Demand)
	if err != nil {
		t.Fatal(err)
	}
	view := execution.Inspect(nil, payload)
	listed := c2j.Job{ID: "fixture-job", Tenant: "fixture-tenant", Execution: &c2j.View{Status: "not_loaded"}}
	claimed := listed
	claimed.Execution = &view
	cfg := &config.Config{Call: 5 * time.Second, Batch: 10 * time.Second, Cool: time.Minute, Lease: time.Minute, BatchSize: 1, PerTenant: 1, MaxPages: 1, Allocation: api.Allocation{Base: "ubuntu-26*", CPUMillis: 1000, MemoryBytes: 1 << 30}, Providers: map[string]config.Provider{"mac": {Type: "orchard"}, "fallback": {Type: "remote"}}}
	cfg.Defaults.Routes = []c2j.Route{{JobType: "recipe"}}
	services := []config.Service{{Name: "mac", Priority: 1}}
	if fallback {
		services = append(services, config.Service{Name: "fallback", Priority: 2})
	}
	cfg.Targets = []config.Target{{Instance: "fixture-db", Tenant: listed.Tenant, JobDB: "https://fixture.invalid/fixture-tenant", Services: services}}
	s := scheduler.New(time.Minute, 5*time.Second)
	localNow := time.Now().UTC().Add(time.Hour)
	s.Now = func() time.Time { return localNow }
	h.controller = &controller.Controller{Config: cfg, Scheduler: s, Providers: map[string]api.Provider{"mac": h.host, "fallback": h.fallback}, Lister: tartDiagnosticList{listed}, Claimer: tartDiagnosticClaimer{claimed, h.lease}, Log: slog.New(slog.NewJSONHandler(&h.log, nil))}
	t.Cleanup(h.host.close)
	return h
}
func (h *tartDiagnosticHarness) once(t *testing.T, ctx context.Context) (tartOperatorRecord, []json.RawMessage) {
	t.Helper()
	h.log.Reset()
	if err := h.controller.Once(ctx); err != nil {
		t.Fatal(err)
	}
	var records []json.RawMessage
	var result tartOperatorRecord
	launches := 0
	dec := json.NewDecoder(bytes.NewReader(h.log.Bytes()))
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, raw)
		var message struct {
			Message string `json:"msg"`
		}
		if err = json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		if message.Message == "launch result" {
			if err = json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			launches++
			t.Logf("operator record: %s", raw)
		}
	}
	if launches != 1 {
		t.Fatalf("expected one correlated placement result, got %d", launches)
	}
	return result, records
}
func tartFactTime(t *testing.T, f placement.Fact) time.Time {
	t.Helper()
	s, ok := f.Value.(string)
	if f.State != "known" || !ok {
		t.Fatalf("missing actual timestamp: %+v", f)
	}
	stamp, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}
func assertTartDiagnosticRecord(t *testing.T, h *tartDiagnosticHarness, r tartOperatorRecord, status api.Status, hasFallback bool) {
	t.Helper()
	if len(h.runtime.launches) != 1 || len(h.runtime.results) != 1 {
		t.Fatal("runtime calls", len(h.runtime.launches), len(h.runtime.results))
	}
	l, original := h.runtime.launches[0], h.runtime.results[0]
	if r.Message != "launch result" || r.Level != "INFO" || r.Instance != l.Job.InstanceID || r.Tenant != l.Job.TenantID || r.Job != l.Job.JobID || r.LaunchID != l.LaunchID || r.Status != status {
		t.Fatal("correlations/status", r)
	}
	if r.Requested.Base.Value != l.Base || r.Requested.Platform.State != "unknown" || r.Requested.Platform.Value != nil || r.Requested.CPUMillis.Value != float64(l.CPUMillis) || r.Requested.MemoryBytes.Value != float64(l.MemoryBytes) || r.Requested.ScratchBytes.State != "unspecified" || r.Requested.ScratchBytes.Value != nil {
		t.Fatal("allocation differs from actual provider input", r.Requested)
	}
	caps, _ := json.Marshal(l.Capabilities)
	loggedCaps, _ := json.Marshal(r.Requested.Capabilities.Value)
	if !bytes.Equal(caps, loggedCaps) {
		t.Fatal("capability request lost", r.Requested)
	}
	count := 1
	if hasFallback {
		count = 2
	}
	if len(r.Attempts) != count {
		t.Fatal(r.Attempts)
	}
	a := r.Attempts[0]
	if a.Service != "mac" || a.ProviderType != "orchard" || !a.SubmitCalled || len(a.OriginalResults) != 1 || !reflect.DeepEqual(a.OriginalResults[0], original) || !reflect.DeepEqual(a.Result, original) || !reflect.DeepEqual(a.SubmissionError, placement.Error(h.runtime.submitError)) || a.SubmissionErrorScope != "launch" || a.DiscoveryError != nil || a.CapabilityValidationError != nil || a.NormalizationError != nil {
		t.Fatal("original runtime diagnostic changed", a)
	}
	if !reflect.DeepEqual(a.ProviderPreparation, placement.ReturnedProviderPreparation(h.host.maintenanceError)) {
		t.Fatal("provider preparation observation changed", a.ProviderPreparation)
	}
	if h.runtime.submitError != nil {
		t.Fatal("fixture should retain per-item Reason separately from nil method error", h.runtime.submitError)
	}
	if h.runtime.discoveries != 1 || h.runtime.discoveryProbed {
		t.Fatal("discovery added backend calls", h.runtime.discoveries, h.runtime.discoveryProbed)
	}
	for _, f := range []placement.Fact{a.Capacity.Occupied, a.Capacity.AvailableWorkers, a.Capacity.ResourceLimits, a.Capacity.AdmissionConstraint} {
		if f.State != "unknown" || f.Value != nil {
			t.Fatal("invented capacity", f)
		}
	}
	service := "mac"
	if hasFallback {
		service = "fallback"
	}
	if r.Service != service || r.Reason != r.Attempts[count-1].Result.Reason {
		t.Fatal("final meaning changed", r)
	}
	if hasFallback {
		if len(h.fallback.launches) != 1 || !reflect.DeepEqual(l, h.fallback.launches[0]) || r.Attempts[1].Service != "fallback" {
			t.Fatal("fallback changed request", r.Attempts)
		}
	} else if len(h.fallback.launches) != 0 {
		t.Fatal("unexpected fallback")
	}
	if status == api.Accepted || status == api.Unknown {
		admission := "accepted"
		if status == api.Unknown {
			admission = "uncertain"
		}
		if r.Admission != admission || len(h.lease.requests) != 0 || r.Retry.Release.Outcome != "not_attempted" || r.Retry.Cooldown.State != "unspecified" || r.Retry.NextEligibleAt.Value != nil || len(h.controller.Scheduler.Cooldowns()) != 0 {
			t.Fatal("ownership/retry changed", r)
		}
	} else {
		if r.Admission != "not_accepted" || len(h.lease.requests) != 1 || r.Retry.Release.Outcome != "acknowledged" || r.Retry.Cooldown.Value != "1m0s" {
			t.Fatal("definite refusal retry", r)
		}
		applied := h.lease.requests[0].WaitUntil
		if applied == nil || !tartFactTime(t, r.Retry.Release.RequestedUntil).Equal(*applied) || !tartFactTime(t, r.Retry.Release.AcknowledgedUntil).Equal(*applied) {
			t.Fatal("deadline not actual Reschedule")
		}
		cs := h.controller.Scheduler.Cooldowns()
		if len(cs) != 1 || !tartFactTime(t, r.Retry.LocalEligibleAt).Equal(cs[0].EligibleAt) {
			t.Fatal("local deadline not stored cooldown", cs)
		}
		next := cs[0].EligibleAt
		if applied.After(next) {
			next = *applied
		}
		if !tartFactTime(t, r.Retry.NextEligibleAt).Equal(next) {
			t.Fatal("combined gate wrong", r.Retry)
		}
	}
	transcript := strings.Join(h.runtime.fixture.Transcript(), "\n")
	for _, secret := range []string{string(l.Lease), base64.StdEncoding.EncodeToString([]byte(l.Lease)), "fixture unrelated output", "PULSE_ENVELOPE", "pulse-tart-guest ingest", `"environment"`, `"lease"`} {
		if strings.Contains(h.log.String(), secret) || strings.Contains(transcript, secret) {
			t.Fatal("captured unrelated payload")
		}
	}
}

func TestTartDiagnosticD01(t *testing.T) {
	for _, scenario := range []string{"inventory", "mixed"} {
		for _, fallback := range []bool{false, true} {
			name := scenario
			if fallback {
				name += "/fallback"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				f := df.NewTartDiagnosticFixture(t, false)
				if err := f.Ready(ctx); err != nil {
					t.Fatal(err)
				}
				if scenario == "mixed" {
					if err := f.Mixed(ctx); err != nil {
						t.Fatal(err)
					}
				}
				before := f.Snapshot()
				f.Scenario(scenario)
				h := newTartDiagnosticHarness(t, f, fallback)
				r, _ := h.once(t, ctx)
				status := api.Unavailable
				if scenario == "mixed" {
					status = api.NoCapacity
				}
				if fallback {
					status = api.Accepted
				}
				assertTartDiagnosticRecord(t, h, r, status, fallback)
				reason := h.runtime.results[0].Reason
				if scenario == "inventory" {
					if !strings.Contains(reason, df.TartDiagnosticInventory) || !strings.Contains(reason, "503") {
						t.Fatal("concrete inventory error lost", reason)
					}
				} else {
					for _, gate := range []string{"reservation occupied", "source-referencing clone", "suspension invalid; quarantined"} {
						if !strings.Contains(reason, gate) {
							t.Fatalf("missing reached gate %q: %s", gate, reason)
						}
					}
				}
				if f.Snapshot()["POST job"] != before["POST job"] {
					t.Fatal("refusal dispatched compute")
				}
				if !fallback {
					counts := f.Snapshot()
					h.log.Reset()
					if err := h.controller.Once(ctx); err != nil {
						t.Fatal(err)
					}
					if h.log.Len() != 0 || len(h.runtime.launches) != 1 || !reflect.DeepEqual(counts, f.Snapshot()) {
						t.Fatal("cooldown allowed early retry")
					}
				}
			})
		}
	}
}
func TestTartDiagnosticD02(t *testing.T) {
	for _, scenario := range []string{"preparation", "create", "accepted", "readiness-error", "handoff-error", "docker-accepted"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			docker := strings.Contains(scenario, "readiness") || strings.Contains(scenario, "handoff") || scenario == "docker-accepted"
			f := df.NewTartDiagnosticFixture(t, docker)
			var preparationErr error
			if scenario == "preparation" {
				f.Scenario(scenario)
				preparationErr = f.Runtime.Reconcile(ctx)
				if preparationErr == nil || !strings.Contains(preparationErr.Error(), df.TartDiagnosticPreparation) || !strings.Contains(preparationErr.Error(), "503") {
					t.Fatal("real preparation error absent", preparationErr)
				}
			} else {
				if err := f.Ready(ctx); err != nil {
					t.Fatal(err)
				}
				f.Scenario(scenario)
			}
			before := f.Snapshot()
			h := newTartDiagnosticHarness(t, f, scenario != "preparation")
			h.host.maintenanceError = preparationErr
			r, records := h.once(t, ctx)
			status := api.Unknown
			if scenario == "accepted" || scenario == "docker-accepted" || scenario == "readiness-error" {
				status = api.Accepted
			}
			if scenario == "preparation" {
				status = api.NoCapacity
			}
			// Readiness refusals may fall back. The preparation case instead exercises
			// correlated no-capacity backoff with a concrete raw provider error.
			// Unknown creates and handoffs must never fall back or release the lease.
			fallback := scenario == "readiness-error"
			assertTartDiagnosticRecord(t, h, r, status, fallback)
			reason := h.runtime.results[0].Reason
			posts := f.Snapshot()["POST job"] - before["POST job"]
			expectedPosts := 0
			switch scenario {
			case "preparation":
				found := false
				for _, raw := range records {
					var record struct {
						Message string `json:"msg"`
						Error   string `json:"error"`
					}
					if err := json.Unmarshal(raw, &record); err != nil {
						t.Fatal(err)
					}
					if record.Message == "provider maintenance deferred" {
						found = true
						if record.Error != preparationErr.Error() {
							t.Fatal("saved preparation error changed")
						}
					}
				}
				if !found {
					t.Fatal("actual preparation error not logged")
				}
				observed := r.Attempts[0].ProviderPreparation
				if observed.Observation != "error_returned" || observed.Scope != "provider" || observed.Error == nil || *observed.Error != preparationErr.Error() {
					t.Fatal("original provider preparation error missing from correlated refusal", observed)
				}
				// Recover through the real setup, not a journal edit. The new accepted
				// placement must not retain the resolved preparation cause.
				f.Scenario("")
				if err := f.Ready(ctx); err != nil {
					t.Fatal(err)
				}
				h.host.maintenanceError = nil
				eligible := tartFactTime(t, r.Retry.NextEligibleAt).Add(time.Second)
				h.controller.Scheduler.Now = func() time.Time { return eligible }
				recovered, _ := h.once(t, ctx)
				if recovered.Status != api.Accepted || recovered.Reason != "" || len(recovered.Attempts) != 1 || recovered.Attempts[0].OriginalResults[0].Reason != "" || len(h.lease.requests) != 1 {
					t.Fatal("resolved preparation cause retained", recovered)
				}
				return
			case "create":
				expectedPosts = 1
				if !strings.Contains(reason, df.TartDiagnosticCreate) || !strings.Contains(reason, "503") {
					t.Fatal("native create cause lost", reason)
				}
			case "accepted":
				expectedPosts = 1
				fallthrough
			case "docker-accepted":
				if reason != "" {
					t.Fatal("accepted reason changed", reason)
				}
			case "readiness-error", "handoff-error":
				if !strings.Contains(reason, "Process exited with status 23") {
					t.Fatal("concrete SSH cause lost", reason)
				}
			}
			if posts != expectedPosts {
				t.Fatalf("native dispatch count %d, want %d", posts, expectedPosts)
			}
			if !fallback {
				beforeReplay := f.Snapshot()
				replay, err := f.Runtime.Submit(ctx, []api.Launch{h.runtime.launches[0]})
				if err != nil || len(replay) != 1 || replay[0].Status != status || !reflect.DeepEqual(beforeReplay, f.Snapshot()) {
					t.Fatal("identical launch replay changed ownership or called backend", replay, err)
				}
				if status == api.Accepted && replay[0].Reason != "" {
					t.Fatal("accepted replay reason changed")
				}
			}
			beforeClose := f.Snapshot()
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			for key, n := range f.Snapshot() {
				if strings.HasPrefix(key, "DELETE ") && n != beforeClose[key] {
					t.Fatal("closing diagnostic fixture deleted compute")
				}
			}
		})
	}
}
