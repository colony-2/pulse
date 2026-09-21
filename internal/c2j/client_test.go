package c2j

import (
	"context"
	"encoding/json"
	"github.com/colony-2/cortex/pkg/compute"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestListExecutableContract(t *testing.T) {
	c := Client{Run: func(_ context.Context, args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "compatible") {
			t.Fatal(args)
		}
		if !reflect.DeepEqual(args[len(args)-2:], []string{"--page-token", "opaque"}) {
			t.Fatal(args)
		}
		return []byte(`{"jobs":[],"next_page_token":"next"}`), nil
	}}
	p, e := c.List(context.Background(), "https://db/t", "github.com/acme/app", "opaque")
	if e != nil || p.Next != "next" {
		t.Fatal(p, e)
	}
}
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
	p, e := Process("https://db/t", "job", "launch", compute.Allocation{CPUMillis: 1500, MemoryBytes: 1025, ScratchBytes: 2048, Image: "runner:1", Platform: "linux/amd64"}, nil, map[string]string{"cortex_job_id": "job"})
	if e != nil || p.Env["C2J_EXECUTION_CPU"] != "1500m" || p.Env["CORTEX_JOB_ID"] != "job" {
		t.Fatal(p, e)
	}
	_, e = Process("", "", "", compute.Allocation{}, map[string]string{"C2J_EXECUTION_MEMORY": "1Gi"}, nil)
	if e == nil {
		t.Fatal("allocation override accepted")
	}
}
