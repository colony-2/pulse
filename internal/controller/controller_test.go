package controller

import (
	"context"
	"errors"
	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/jobdb/pkg/jobdb"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/colony-2/pulse/pkg/compute"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type claimFunc func(context.Context, string, c2j.Job, string, time.Duration) (*c2j.Claimed, error)

func (f claimFunc) Claim(ctx context.Context, uri string, j c2j.Job, worker string, duration time.Duration) (*c2j.Claimed, error) {
	return f(ctx, uri, j, worker, duration)
}

type testLease struct{ jobdb.ExecutionLease }

func (testLease) Route() jobdb.Route                                                 { return jobdb.Route{JobType: "recipe"} }
func (testLease) ExecutionState() jobdb.ExecutionState                               { return jobdb.ExecutionState{} }
func (testLease) Reschedule(context.Context, jobdb.RescheduleExecutionRequest) error { return nil }

type listFunc func(context.Context, string, string) (c2j.Page, error)

func (f listFunc) List(c context.Context, u, token string) (c2j.Page, error) {
	return f(c, u, token)
}

type accept struct {
	batchSizes []int
	requests   []compute.Request
	process    []compute.Process
}

func (p *accept) Submit(_ context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	p.batchSizes = append(p.batchSizes, len(ls))
	out := []compute.Submission{}
	for _, l := range ls {
		p.requests = append(p.requests, l.Request)
		p.process = append(p.process, l.Process)
		out = append(out, compute.Submission{LaunchID: l.LaunchID, Status: compute.Accepted})
	}
	return out, nil
}
func TestTenantIsolationPaginationAndLeaseOwnership(t *testing.T) {
	p := &accept{}
	cfg := &config.Config{Call: time.Second, Batch: time.Second, Cool: time.Minute, BatchSize: 1, PerTenant: 10, MaxPages: 10, ExecutionTimeout: time.Hour, Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: "registry.example/runner:1", Platform: "linux/arm64"}}
	cfg.Defaults.Routes = []c2j.Route{{JobType: "recipe"}}
	cfg.Targets = []config.Target{{Instance: "db", JobDB: "https://db/t", Tenant: "t", Services: []config.Service{{Name: "p", Priority: 1}}}}
	cfg.Targets = append([]config.Target{{Instance: "db", JobDB: "https://db/bad", Tenant: "bad", Services: cfg.Targets[0].Services}}, cfg.Targets...)
	calls := 0
	list := listFunc(func(_ context.Context, uri, token string) (c2j.Page, error) {
		if uri == "https://db/bad" {
			return c2j.Page{}, errors.New("offline")
		}
		calls++
		id := "a"
		next := "second"
		if token != "" {
			id = "b"
			next = ""
		}
		return c2j.Page{Jobs: []c2j.Job{{Tenant: "t", ID: id, Repository: "github.com/acme/" + id, Status: "READY", Next: &c2j.Route{JobType: "recipe"}, Execution: &c2j.View{Status: "unresolved", Source: "absent"}}}, Next: next}, nil
	})
	claimed := map[string]bool{}
	claimer := claimFunc(func(_ context.Context, _ string, job c2j.Job, _ string, _ time.Duration) (*c2j.Claimed, error) {
		if claimed[job.ID] {
			return nil, nil
		}
		claimed[job.ID] = true
		memory := "8Gi"
		demand, err := execution.Initial(nil, "digest", execution.Requirements{Resources: execution.Resources{Memory: &memory}})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := execution.PayloadWithDemand(nil, demand)
		if err != nil {
			t.Fatal(err)
		}
		view := execution.Inspect(nil, payload)
		job.Execution = &view
		return &c2j.Claimed{Job: job, Lease: testLease{}, Encoded: []byte("private-capability")}, nil
	})
	c := Controller{Config: cfg, Lister: list, Claimer: claimer, Scheduler: scheduler.New(time.Minute, time.Second), Providers: map[string]compute.Provider{"p": p}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if c.Once(context.Background()) == nil {
		t.Fatal("lost tenant failure")
	}
	if len(p.requests) != 2 || calls != 2 {
		t.Fatal(p.requests, calls)
	}
	if p.requests[0].Metadata["pulse_job_id"] != "a" || p.requests[1].Metadata["pulse_job_id"] != "b" {
		t.Fatal("wrong closures")
	}
	if p.requests[0].MemoryBytes != 8<<30 || p.process[0].Env["C2J_EXECUTION_MEMORY"] != "8388608Ki" || p.process[0].Stdin != "private-capability" {
		t.Fatal("did not use claimed allocation or lease")
	}
	if p.process[1].Env["PULSE_JOB_ID"] != "b" {
		t.Fatal(p.process)
	}
	c.Once(context.Background())
	if len(p.requests) != 2 {
		t.Fatal("already leased job launched again")
	}
}

func (p *accept) List(context.Context, compute.ListRequest) (compute.ListResponse, error) {
	return compute.ListResponse{Items: []compute.Instance{}}, nil
}

func TestConfiguredBatchSizeBoundsClaimCount(t *testing.T) {
	p := &accept{}
	cfg := &config.Config{Call: time.Second, Batch: time.Second, Lease: time.Minute, Cool: time.Minute, BatchSize: 2, PerTenant: 7, MaxPages: 1, ExecutionTimeout: time.Hour, Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: "runner:1", Platform: "linux/amd64"}}
	cfg.Defaults.Routes = []c2j.Route{{JobType: "recipe"}}
	cfg.Targets = []config.Target{{Instance: "db", JobDB: "https://db/t", Tenant: "t", Services: []config.Service{{Name: "p", Priority: 1}}}}
	var claims atomic.Int32
	list := listFunc(func(context.Context, string, string) (c2j.Page, error) {
		page := c2j.Page{}
		for i := range 7 {
			page.Jobs = append(page.Jobs, c2j.Job{Tenant: "t", ID: string(rune('a' + i)), Status: "READY", Next: &c2j.Route{JobType: "recipe"}, Execution: &c2j.View{Status: "unresolved", Source: "absent"}})
		}
		return page, nil
	})
	claimer := claimFunc(func(_ context.Context, _ string, j c2j.Job, _ string, _ time.Duration) (*c2j.Claimed, error) {
		claims.Add(1)
		return &c2j.Claimed{Job: j, Lease: testLease{}, Encoded: []byte("private-capability")}, nil
	})
	c := Controller{Config: cfg, Lister: list, Claimer: claimer, Scheduler: scheduler.New(time.Minute, time.Second), Providers: map[string]compute.Provider{"p": p}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := c.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if claims.Load() != 7 || len(p.batchSizes) != 4 {
		t.Fatal("incorrect claim batching", claims.Load(), p.batchSizes)
	}
	for i, size := range p.batchSizes {
		want := 2
		if i == 3 {
			want = 1
		}
		if size != want {
			t.Fatal("incorrect batch size", p.batchSizes)
		}
	}
}
