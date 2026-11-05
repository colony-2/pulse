package c2j

import (
	"encoding/json"
	"github.com/colony-2/c2j/pkg/execution"
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
	"github.com/colony-2/pulse/pkg/compute"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPackageBearingDemandUsesC2JProjection(t *testing.T) {
	base := execution.Requirements{Packages: []string{"uv:pyfiglet==1.0.2"}}
	demand, err := execution.Initial(&base, true, execution.Requirements{})
	if err != nil {
		t.Fatal(err)
	}
	node := execution.Requirements{Packages: []string{"pnpm:typescript@5.8.3"}}
	demand.Effective = execution.Overlay(base, node)
	payload, err := execution.PayloadWithDemand(nil, demand.Demand)
	if err != nil {
		t.Fatal(err)
	}
	view := execution.Inspect(json.RawMessage(`{}`), payload)
	if view.Diagnostic != "" || view.Demand == nil || len(view.Demand.Effective.Packages) != 2 {
		t.Fatal(view)
	}
	job := Job{Tenant: "tenant", ID: "job", Execution: &view}
	want := provider.Allocation{Base: "oci:linux/arm64:ghcr.io/colony-2/base2:latest", CPUMillis: 1000, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30}
	got, err := job.Allocation(want)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
}

func TestDemandAndRoute(t *testing.T) {
	var j Job
	err := json.Unmarshal([]byte(`{"tenant_id":"t","job_id":"j","status":"READY","next_route":{"jobType":"recipe","taskType":"input:with, spaces"},"execution":{"status":"unresolved","source":"submission","demand":{"schema_version":3,"recipe_resolution":"unresolved","effective":{"resources":{"memory":"4Gi"}}}}}`), &j)
	if err != nil {
		t.Fatal(err)
	}
	if j.Next == nil || j.Next.TaskType != "input:with, spaces" {
		t.Fatal("opaque route not preserved")
	}
	a, e := j.Allocation(provider.Allocation{CPUMillis: 1000, MemoryBytes: 1024, ScratchBytes: 1024, Base: "oci:linux/amd64:docker.io/library/alpine:3"})
	if e != nil || a.MemoryBytes != 4<<30 || a.Base != "oci:linux/amd64:docker.io/library/alpine:3" {
		t.Fatal(a, e)
	}
	j.Execution.Status = "unsupported"
	if _, e = j.Allocation(a); e == nil {
		t.Fatal("unsupported treated as empty")
	}
	j.Execution = nil
	if _, e = j.Allocation(a); e == nil {
		t.Fatal("missing view accepted")
	}
}
func TestUnspecifiedJobResources(t *testing.T) {
	for _, raw := range []string{
		`{"tenant_id":"t","job_id":"j","execution":{"status":"unresolved"}}`,
		`{"tenant_id":"t","job_id":"j","execution":{"status":"unspecified","demand":{"schema_version":3,"recipe_resolution":"resolved","effective":{}}}}`,
		`{"tenant_id":"t","job_id":"j","execution":{"status":"specified","demand":{"schema_version":3,"recipe_resolution":"resolved","effective":{"resources":{"memory":"4Gi"}}}}}`,
	} {
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			t.Fatal(err)
		}
		a, err := job.Allocation(provider.Allocation{Base: "oci:linux/arm64:ghcr.io/colony-2/base2:latest"})
		if err != nil || a.CPUMillis != 0 || a.ScratchBytes != 0 {
			t.Fatal(a, err)
		}
		b, err := bases.Resolve(a.Base, nil)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Process("https://db/t", "j", compute.Allocation{Image: b.Image, Platform: b.Platform, CPUMillis: a.CPUMillis, MemoryBytes: a.MemoryBytes, ScratchBytes: a.ScratchBytes}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"C2J_EXECUTION_CPU", "C2J_EXECUTION_EPHEMERAL_STORAGE"} {
			if _, ok := p.Env[key]; ok {
				t.Fatal("unspecified resource advertised", p.Env)
			}
		}
		if strings.Contains(raw, "4Gi") {
			if a.MemoryBytes != 4<<30 || p.Env["C2J_EXECUTION_MEMORY"] != "4194304Ki" {
				t.Fatal(a, p.Env)
			}
		} else if _, ok := p.Env["C2J_EXECUTION_MEMORY"]; ok || a.MemoryBytes != 0 {
			t.Fatal(a, p.Env)
		}
	}
}

func TestRecordedLegacyC2JListRejected(t *testing.T) {
	b, e := os.ReadFile("testdata/list-v0.0.52.json")
	if e != nil {
		t.Fatal(e)
	}
	var page Page
	e = json.Unmarshal(b, &page)
	if e != nil || len(page.Jobs) != 1 {
		t.Fatal(page, e)
	}
	_, e = page.Jobs[0].Allocation(provider.Allocation{CPUMillis: 250, MemoryBytes: 1 << 20, ScratchBytes: 1 << 20, Base: "oci:linux/arm64:registry.example/runner:1"})
	if e == nil || !strings.Contains(e.Error(), "unsupported execution schema version 1") {
		t.Fatal("legacy demand accepted", e)
	}
}

func TestAllocationProcess(t *testing.T) {
	p, e := Process("https://db/t", "job", compute.Allocation{CPUMillis: 1500, MemoryBytes: 1025, ScratchBytes: 2048, Image: "runner:1", Platform: "linux/amd64"}, nil, map[string]string{"pulse_job_id": "job"})
	if e != nil || p.Env["C2J_EXECUTION_CPU"] != "1500m" || p.Env["PULSE_JOB_ID"] != "job" {
		t.Fatal(p, e)
	}
	_, e = Process("", "", compute.Allocation{}, map[string]string{"C2J_EXECUTION_MEMORY": "1Gi"}, nil)
	if e == nil {
		t.Fatal("allocation override accepted")
	}
}

func TestRequestedPinnedImagePassesC2JCompatibility(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	image := "registry.example/runner@" + digest
	for _, ref := range []string{image, "registry.example/runner:1"} {
		a := compute.Allocation{CPUMillis: 1500, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: ref, Platform: "linux/amd64", ImageDigest: "sha256:" + strings.Repeat("b", 64), ImageID: "diagnostic"}
		p, err := Process("https://db/t", "job", a, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := p.Env["C2J_EXECUTION_IMAGE_ID"]; ok {
			t.Fatal("provider image ID leaked into request", p.Env)
		}
		if ref == image && p.Env["C2J_EXECUTION_IMAGE_DIGEST"] != digest {
			t.Fatal(p.Env)
		}
		if ref != image && p.Env["C2J_EXECUTION_IMAGE_DIGEST"] != "" {
			t.Fatal("unrequested digest", p.Env)
		}
		mismatches, err := execution.Compare(execution.Requirements{Image: &ref}, execution.Allocation{SchemaVersion: execution.SchemaVersion, Image: execution.Image{Reference: p.Env["C2J_EXECUTION_IMAGE"], ManifestDigest: p.Env["C2J_EXECUTION_IMAGE_DIGEST"]}})
		if err != nil || len(mismatches) != 0 {
			t.Fatal(mismatches, err)
		}
	}
}

func TestCapabilitiesAndBaseDefaultFromClaimedDemand(t *testing.T) {
	image, platform := "alpine:3", "linux/amd64"
	ubuntu, macos, custom := "ubuntu-26*arm64", "macos*", "custom-runner"
	for _, tt := range []struct {
		req  execution.Requirements
		want string
	}{
		{execution.Requirements{Base: &ubuntu}, ubuntu},
		{execution.Requirements{Base: &macos, Capabilities: []string{"git", "nix"}}, macos},
		{execution.Requirements{Base: &custom}, custom},
		{execution.Requirements{Capabilities: []string{"docker"}}, bases.Default},
		{execution.Requirements{Platform: &platform}, "ubuntu-26.04-amd64"},
		{execution.Requirements{Image: &image, Platform: &platform}, "oci:linux/amd64:docker.io/library/alpine:3"},
	} {
		demand, err := execution.Initial(nil, true, tt.req)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := execution.PayloadWithDemand(nil, demand.Demand)
		if err != nil {
			t.Fatal(err)
		}
		view := execution.Inspect(nil, payload)
		job := Job{Tenant: "tenant", ID: "job", Execution: &view}
		got, err := job.Allocation(provider.Allocation{Base: bases.Default})
		if err != nil || got.Base != tt.want || !reflect.DeepEqual(got.Capabilities, tt.req.Capabilities) {
			t.Fatal(got, err)
		}
	}
}

func TestInvalidBaseDemandRejected(t *testing.T) {
	base, image, platform := "ubuntu*", "alpine:3", "linux/arm64"
	for _, tt := range []struct {
		name    string
		version int
		req     execution.Requirements
	}{
		{"base and image", execution.DemandSchemaVersion, execution.Requirements{Base: &base, Image: &image}},
		{"base and platform", execution.DemandSchemaVersion, execution.Requirements{Base: &base, Platform: &platform}},
		{"base in legacy schema", 3, execution.Requirements{Base: &base}},
		{"unknown schema", 99, execution.Requirements{Base: &base}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			j := Job{Tenant: "tenant", ID: "job", Execution: &View{Status: "specified", Demand: &execution.Demand{SchemaVersion: tt.version, RecipeResolution: "resolved", Effective: tt.req}}}
			if _, err := j.Allocation(provider.Allocation{Base: bases.Default}); err == nil {
				t.Fatal("invalid demand fell back to defaults")
			}
		})
	}
}
