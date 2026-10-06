package c2j

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/jobdb/pkg/jobdb"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/toy"
	"github.com/colony-2/pulse/pkg/compute"
)

func TestClaimExportSnapshotAndRelease(t *testing.T) {
	ctx := context.Background()
	backend := toy.New()
	memory := "8Gi"
	demand, err := execution.Initial(nil, "recipe-digest", execution.Requirements{Resources: execution.Resources{Memory: &memory}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := execution.PayloadWithDemand(nil, demand)
	if err != nil {
		t.Fatal(err)
	}
	job, err := backend.SubmitJob(ctx, jobdb.SubmitJobRequest{Job: jobdb.SubmitJob{TenantId: "tenant", JobType: "recipe", Data: jobdb.NewTaskDataOrPanic(1), ClientPayloadUpdate: &jobdb.ClientPayloadUpdate{Mode: "reset", Value: payload}}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(remote.NewServer(backend))
	defer server.Close()
	uri := server.URL + "/tenant"
	client, err := NewEmbedded([]Connection{{URI: uri}})
	if err != nil {
		t.Fatal(err)
	}
	listed := Job{Tenant: "tenant", ID: job.JobKey.JobId, Next: &Route{JobType: "recipe"}, Execution: &View{Status: "unresolved"}}
	claimed, err := client.Claim(ctx, uri, listed, "launch-owner", time.Minute)
	if err != nil || claimed == nil {
		t.Fatal(err)
	}
	a, err := claimed.Job.Allocation(compute.Allocation{CPUMillis: 1000, MemoryBytes: 1024, ScratchBytes: 1024, Image: "runner:1", Platform: "linux/amd64"})
	if err != nil || a.MemoryBytes != 8<<30 {
		t.Fatal("did not use leased demand", a, err)
	}
	if another, err := client.Claim(ctx, uri, listed, "other-owner", time.Minute); err != nil || another != nil {
		t.Fatal("duplicate lease", err)
	}
	capability, err := remote.DecodeLeaseCapability(claimed.Encoded)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := remote.New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	imported, err := runtime.ImportLease(ctx, capability)
	if err != nil {
		t.Fatal(err)
	}
	if imported.LeaseID() != claimed.Lease.LeaseID() || imported.(interface{ LeaseWorkerID() string }).LeaseWorkerID() != "launch-owner" {
		t.Fatal("handoff changed ownership")
	}
	if strings.Contains(fmt.Sprintf("%+v", claimed), string(claimed.Encoded)) {
		t.Fatal("credential leaked in diagnostics")
	}
	if err := claimed.Release(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if another, err := client.Claim(ctx, uri, listed, "retry", time.Minute); err != nil || another != nil {
		t.Fatal("persistent backoff ignored", err)
	}
	if _, err := runtime.ImportLease(ctx, capability); err == nil {
		t.Fatal("released capability still authorized")
	}
}
