package controller

import (
	"context"
	"errors"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/colony-2/pulse/pkg/compute"
	"io"
	"log/slog"
	"testing"
	"time"
)

type listFunc func(context.Context, string, string, string) (c2j.Page, error)

func (f listFunc) List(c context.Context, u, cell, token string) (c2j.Page, error) {
	return f(c, u, cell, token)
}

type accept struct {
	requests []compute.Request
	process  []compute.Process
}

func (p *accept) Submit(_ context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	out := []compute.Submission{}
	for _, l := range ls {
		p.requests = append(p.requests, l.Request)
		p.process = append(p.process, l.Process)
		out = append(out, compute.Submission{LaunchID: l.LaunchID, Status: compute.Accepted})
	}
	return out, nil
}
func TestCellIsolationPaginationAndCooldown(t *testing.T) {
	p := &accept{}
	cfg := &config.Config{Call: time.Second, Batch: time.Second, Cool: time.Minute, BatchSize: 1, PerCell: 10, MaxPages: 10, ExecutionTimeout: time.Hour, Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: "registry.example/runner:1", Platform: "linux/arm64"}}
	cfg.Defaults.Routes = []c2j.Route{{JobType: "recipe"}}
	cfg.Targets = []config.Target{{Instance: "db", JobDB: "https://db/t", Tenant: "t", Cells: []string{"bad", "good"}, Services: []config.Service{{Name: "p", Priority: 1}}}}
	calls := 0
	list := listFunc(func(_ context.Context, _, cell, token string) (c2j.Page, error) {
		if cell == "bad" {
			return c2j.Page{}, errors.New("offline")
		}
		calls++
		id := "a"
		next := "second"
		if token != "" {
			id = "b"
			next = ""
		}
		return c2j.Page{Jobs: []c2j.Job{{Tenant: "t", ID: id, Status: "READY", Next: &c2j.Route{JobType: "recipe"}, Execution: &c2j.View{Status: "unresolved", Source: "absent"}}}, Next: next}, nil
	})
	c := Controller{Config: cfg, Lister: list, Scheduler: scheduler.New(time.Minute, time.Second), Providers: map[string]compute.Provider{"p": p}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if c.Once(context.Background()) == nil {
		t.Fatal("lost cell failure")
	}
	if len(p.requests) != 2 || calls != 2 {
		t.Fatal(p.requests, calls)
	}
	if p.requests[0].Metadata["pulse_job_id"] != "a" || p.requests[1].Metadata["pulse_job_id"] != "b" {
		t.Fatal("wrong closures")
	}
	if p.process[1].Env["PULSE_JOB_ID"] != "b" {
		t.Fatal(p.process)
	}
	c.Once(context.Background())
	if len(p.requests) != 2 {
		t.Fatal("cooldown bypassed")
	}
}

func (p *accept) List(context.Context, compute.ListRequest) (compute.ListResponse, error) {
	return compute.ListResponse{Items: []compute.Instance{}}, nil
}
