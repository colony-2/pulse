package c2j

import (
	"encoding/json"
	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/pulse/pkg/compute"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDemandAndRoute(t *testing.T) {
	var j Job
	err := json.Unmarshal([]byte(`{"tenant_id":"t","job_id":"j","status":"READY","next_route":{"jobType":"recipe","taskType":"input:with, spaces"},"execution":{"status":"unresolved","source":"submission","demand":{"schema_version":1,"effective":{"resources":{"memory":"4Gi"}}}}}`), &j)
	if err != nil {
		t.Fatal(err)
	}
	if j.Ready([]Route{{JobType: "recipe"}}, time.Now()) {
		t.Fatal("human task selected")
	}
	if !j.Ready([]Route{*j.Next}, time.Now()) {
		t.Fatal("opaque route not matched")
	}
	a, e := j.Allocation(compute.Allocation{CPUMillis: 1000, MemoryBytes: 1024, ScratchBytes: 1024, Platform: "linux/amd64", Image: "alpine:3"})
	if e != nil || a.MemoryBytes != 4<<30 || a.Image != "docker.io/library/alpine:3" {
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
func TestAllocationProcess(t *testing.T) {
	p, e := Process("https://db/t", "job", "launch", compute.Allocation{CPUMillis: 1500, MemoryBytes: 1025, ScratchBytes: 2048, Image: "runner:1", Platform: "linux/amd64"}, nil, map[string]string{"pulse_job_id": "job"})
	if e != nil || p.Env["C2J_EXECUTION_CPU"] != "1500m" || p.Env["PULSE_JOB_ID"] != "job" {
		t.Fatal(p, e)
	}
	_, e = Process("", "", "", compute.Allocation{}, map[string]string{"C2J_EXECUTION_MEMORY": "1Gi"}, nil)
	if e == nil {
		t.Fatal("allocation override accepted")
	}
}

func TestRecordedC2JList(t *testing.T) {
	b, e := os.ReadFile("testdata/list-v0.0.52.json")
	if e != nil {
		t.Fatal(e)
	}
	var page Page
	e = json.Unmarshal(b, &page)
	if e != nil || len(page.Jobs) != 1 {
		t.Fatal(page, e)
	}
	a, e := page.Jobs[0].Allocation(compute.Allocation{CPUMillis: 250, MemoryBytes: 1 << 20, ScratchBytes: 1 << 20, Image: "registry.example/runner:1", Platform: "linux/arm64"})
	if e != nil || a.CPUMillis != 1000 || a.MemoryBytes != 1<<30 || a.ScratchBytes != 1<<30 {
		t.Fatal(a, e)
	}
}

func TestRequestedPinnedImagePassesC2JCompatibility(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	image := "registry.example/runner@" + digest
	for _, ref := range []string{image, "registry.example/runner:1"} {
		a := compute.Allocation{CPUMillis: 1500, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: ref, Platform: "linux/amd64", ImageDigest: "sha256:" + strings.Repeat("b", 64), ImageID: "diagnostic"}
		p, err := Process("https://db/t", "job", "launch", a, nil, nil)
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
