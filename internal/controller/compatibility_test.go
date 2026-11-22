package controller

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/scheduler"
)

type catalogProvider struct {
	accept
	catalog     api.Capabilities
	discoveries int
}

func (p *catalogProvider) GetCapabilities(context.Context) (api.Capabilities, error) {
	p.discoveries++
	return p.catalog, nil
}

func listedDemand(t *testing.T, id string, requirements execution.Requirements) c2j.Job {
	t.Helper()
	initial, err := execution.Initial(nil, true, requirements)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := execution.PayloadWithDemand(nil, initial.Demand)
	if err != nil {
		t.Fatal(err)
	}
	view := execution.Inspect(nil, payload)
	return c2j.Job{Tenant: "tenant", ID: id, Execution: &view}
}

func TestControllerFiltersListedDemandBeforeClaiming(t *testing.T) {
	docker := &catalogProvider{catalog: api.Capabilities{Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}}}}
	mac := &catalogProvider{catalog: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "macos-15-arm64"}}}}
	cfg := &config.Config{Call: time.Second, Batch: time.Second, BatchSize: 1, PerTenant: 1, MaxPages: 2, Allocation: api.Allocation{Base: "ubuntu-26*"}}
	cfg.Providers = map[string]config.Provider{"docker": {Type: "docker"}, "mac": {Type: "vz"}}
	cfg.Targets = []config.Target{{Instance: "db", Tenant: "tenant", JobDB: "https://db/tenant", Services: []config.Service{{Name: "docker", Priority: 1}}}}
	macBase, armBase, linuxBase := "macos*", "ubuntu*arm64", "ubuntu*amd64"
	image, platform, ociBase := "alpine:3", "linux/amd64", "oci:linux/amd64:docker.io/library/alpine:3"
	incompatible := []c2j.Job{
		listedDemand(t, "mac", execution.Requirements{Base: &macBase}),
		listedDemand(t, "arm", execution.Requirements{Base: &armBase}),
		listedDemand(t, "nested", execution.Requirements{Base: &linuxBase, Capabilities: []string{"docker"}}),
		listedDemand(t, "image", execution.Requirements{Image: &image}),
		listedDemand(t, "platform", execution.Requirements{Platform: &platform}),
		listedDemand(t, "oci", execution.Requirements{Base: &ociBase}),
	}
	compatible := listedDemand(t, "linux", execution.Requirements{Base: &linuxBase})
	lists := 0
	claims := 0
	c := Controller{
		Config: cfg, Scheduler: scheduler.New(time.Minute, time.Second),
		Providers: map[string]api.Provider{"docker": docker, "mac": mac},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Lister: listFunc(func(_ context.Context, _ string, page string, _ []c2j.Route) (c2j.Page, error) {
			lists++
			if page == "" {
				return c2j.Page{Jobs: incompatible, Next: "next"}, nil
			}
			return c2j.Page{Jobs: []c2j.Job{compatible}}, nil
		}),
		Claimer: claimFunc(func(_ context.Context, _ string, job c2j.Job, _ string, _ time.Duration, _ []c2j.Route) (*c2j.Claimed, error) {
			claims++
			if job.ID != compatible.ID {
				t.Errorf("claimed incompatible job %s", job.ID)
			}
			return &c2j.Claimed{Job: job, Lease: testLease{}, Encoded: []byte("fixture-lease")}, nil
		}),
	}
	if err := c.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if lists != 2 || claims != 1 || len(docker.requests) != 1 || docker.requests[0].Job.JobID != "linux" {
		t.Fatalf("lists=%d claims=%d submissions=%d, expected two pages and one compatible job", lists, claims, len(docker.requests))
	}
	if docker.discoveries != 2 || mac.discoveries != 0 {
		t.Fatalf("discovery calls for docker=%d and mac=%d, expected one pre-claim and one post-claim check of Docker", docker.discoveries, mac.discoveries)
	}
	for _, job := range incompatible {
		if !c.Scheduler.Eligible(scheduler.Key{Instance: "db", Tenant: job.Tenant, Job: job.ID}) {
			t.Fatalf("skipped job %s acquired a cooldown", job.ID)
		}
	}
}
