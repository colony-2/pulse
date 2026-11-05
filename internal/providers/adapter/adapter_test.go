package adapter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/colony-2/c2j/pkg/execution"
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
	"github.com/colony-2/pulse/pkg/compute"
)

type backend struct {
	launches []compute.Launch
	result   []compute.Submission
	err      error
	page     compute.ListResponse
}

func (b *backend) Submit(_ context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	b.launches = append(b.launches, ls...)
	return b.result, b.err
}
func (b *backend) List(context.Context, compute.ListRequest) (compute.ListResponse, error) {
	return b.page, b.err
}
func assignment(id string) provider.Launch {
	return provider.Launch{LaunchID: id, Allocation: provider.Allocation{Base: bases.Default, CPUMillis: 1500, MemoryBytes: 1025, ScratchBytes: 2048}, Job: provider.JobRef{JobDB: "https://db/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}, Lease: "secret-lease"}
}
func TestResolveAndDeliverLeaseWithoutMutatingAssignment(t *testing.T) {
	b := &backend{result: []compute.Submission{{LaunchID: "a", Status: compute.Accepted}}}
	p := &Provider{Native: b, Env: map[string]string{"LITERAL": "${no-expansion}"}}
	l := assignment("a")
	before := l
	out, err := p.Submit(t.Context(), []provider.Launch{l})
	if err != nil || len(out) != 1 || out[0].Status != provider.Accepted || len(b.launches) != 1 {
		t.Fatal(out, err)
	}
	n := b.launches[0]
	if n.Image != bases.Image || n.Platform != "linux/arm64" || n.Process.Stdin != compute.SecretInput(l.Lease) || !reflect.DeepEqual(l, before) {
		t.Fatal("base or lease mapping incorrect")
	}
	want := map[string]string{"C2J_EXECUTION_CPU": "1500m", "C2J_EXECUTION_MEMORY": "1.025k", "C2J_EXECUTION_EPHEMERAL_STORAGE": "2Ki", "C2J_EXECUTION_IMAGE": bases.Image, "C2J_EXECUTION_PLATFORM": "linux/arm64", "PULSE_EXECUTION_BASE": "ubuntu-26.04-arm64", "C2J_EXECUTION_BASE": "ubuntu-26.04-arm64", "C2J_EXECUTION_CAPABILITIES": "[]", "PULSE_JOB_ID": "job", "LITERAL": "${no-expansion}"}
	for k, v := range want {
		if n.Process.Env[k] != v {
			t.Errorf("env %s=%q want %q", k, n.Process.Env[k], v)
		}
	}
	// A later request is compared with the concrete allocation, independently
	// of the selector that Pulse originally admitted.
	actual := execution.Allocation{SchemaVersion: execution.SchemaVersion, Base: n.Process.Env["C2J_EXECUTION_BASE"]}
	for _, tt := range []struct {
		selector string
		matches  bool
	}{
		{l.Base, true}, {"ubuntu*arm64", true}, {actual.Base, true},
		{"ubuntu*amd64", false}, {"linux*", false},
	} {
		mismatches, err := execution.Compare(execution.Requirements{Base: &tt.selector}, actual)
		if err != nil || (len(mismatches) == 0) != tt.matches {
			t.Errorf("worker comparison for %q: %v, %v", tt.selector, mismatches, err)
		}
	}
	for _, v := range append(n.Process.Command, n.Process.Args...) {
		if v == string(l.Lease) {
			t.Fatal("lease in argv")
		}
	}
	for _, v := range n.Process.Env {
		if v == string(l.Lease) {
			t.Fatal("lease in environment")
		}
	}
	if n.Metadata["pulse_jobdb"] != l.Job.JobDB || n.Metadata["pulse_jobdb_instance_id"] != l.Job.InstanceID {
		t.Fatal("correlation lost")
	}
}
func TestMixedDeclinesAndUncertainty(t *testing.T) {
	b := &backend{err: errors.New("lost reply"), result: []compute.Submission{{LaunchID: "ok", Status: compute.Accepted}}}
	p := &Provider{Native: b, Platforms: []string{"linux/amd64"}}
	noArch := assignment("arch")
	noArch.Base = "ubuntu-26*arm64"
	caps := assignment("caps")
	caps.Capabilities = []string{"docker"}
	ok := assignment("ok")
	ok.Base = "ubuntu*"
	out, err := p.Submit(t.Context(), []provider.Launch{noArch, caps, ok})
	if err == nil || len(out) != 3 || len(b.launches) != 1 || b.launches[0].LaunchID != "ok" || b.launches[0].Platform != "linux/amd64" {
		t.Fatal(out, err)
	}
	if out[0].Status != provider.Unsupported || out[1].Status != provider.Unsupported || out[2].Status != provider.Accepted {
		t.Fatal(out)
	}
}
func TestInvalidBatchHasNoNativeSideEffects(t *testing.T) {
	b := &backend{}
	p := &Provider{Native: b}
	l := assignment("same")
	if _, err := p.Submit(t.Context(), []provider.Launch{l, l}); err == nil || len(b.launches) != 0 {
		t.Fatal("duplicate batch processed")
	}
	l.Job.JobDB = "https://user:secret@db/tenant"
	if _, err := p.Submit(t.Context(), []provider.Launch{l}); err == nil || len(b.launches) != 0 {
		t.Fatal("credential-bearing endpoint accepted")
	}
}
func TestListingRestoresStructuredJobWithoutLease(t *testing.T) {
	l := assignment("a")
	m := Metadata(l)
	m["secret"] = "do-not-expose"
	b := &backend{page: compute.ListResponse{Items: []compute.Instance{{ID: "native", LaunchID: "a", State: "running", Metadata: m, Refs: []string{"native"}}}}}
	p := &Provider{Native: b, Targets: map[string]string{"db\x00tenant": l.Job.JobDB}}
	for _, legacy := range []bool{false, true} {
		if legacy {
			delete(m, "pulse_jobdb")
		}
		page, err := p.List(t.Context(), provider.ListRequest{})
		if err != nil || len(page.Items) != 1 || page.Items[0].Job != l.Job {
			t.Fatal(page, err)
		}
	}
	b.err = errors.New("offline")
	if _, err := p.List(t.Context(), provider.ListRequest{}); err == nil {
		t.Fatal("listing error lost")
	}
}

func TestMalformedNativeListingIsNotAnEmptySuccess(t *testing.T) {
	b := &backend{}
	p := &Provider{Native: b}
	if _, err := p.List(t.Context(), provider.ListRequest{}); err == nil {
		t.Fatal("nil page accepted")
	}
	b.page = compute.ListResponse{Items: []compute.Instance{{ID: "orphan", LaunchID: "a", State: "running", Metadata: map[string]string{"pulse_job_id": "job"}, Refs: []string{}}}}
	if _, err := p.List(t.Context(), provider.ListRequest{}); err == nil {
		t.Fatal("missing job identity accepted")
	}
}

func TestCapabilitiesAndResolutionPreferNativePlatform(t *testing.T) {
	for _, native := range []string{"linux/arm64", "linux/amd64"} {
		t.Run(native, func(t *testing.T) {
			alternate := "linux/amd64"
			if native == alternate {
				alternate = "linux/arm64"
			}
			b := &backend{result: []compute.Submission{{LaunchID: "native", Status: compute.Accepted}}}
			p := &Provider{Native: b, Platforms: []string{native, alternate}}
			caps, err := p.GetCapabilities(t.Context())
			if err != nil || caps.BasesComplete || len(caps.Bases) != 2 {
				t.Fatal(caps, err)
			}
			l := assignment("native")
			l.Base = bases.Default
			if _, err = p.Submit(t.Context(), []provider.Launch{l}); err != nil {
				t.Fatal(err)
			}
			if len(b.launches) != 1 || b.launches[0].Platform != native || b.launches[0].Process.Env["C2J_EXECUTION_BASE"] != caps.Bases[0].Name {
				t.Fatal("native preference not shared by discovery and submission", caps, b.launches)
			}
			// An explicit request must still override the preference.
			l.Base = caps.Bases[1].Name
			if _, err = p.Submit(t.Context(), []provider.Launch{l}); err != nil {
				t.Fatal(err)
			}
			if b.launches[1].Platform != alternate {
				t.Fatal("explicit architecture broadened")
			}
		})
	}
}
