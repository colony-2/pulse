package controller

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/jobdb/pkg/jobdb"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/toy"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/colony-2/pulse/pkg/compute"
)

type routeProvider struct {
	accept
	reject bool
}

func (p *routeProvider) Submit(ctx context.Context, launches []compute.Launch) ([]compute.Submission, error) {
	out, err := p.accept.Submit(ctx, launches)
	if p.reject {
		for i := range out {
			out[i].Status = compute.NoCapacity
		}
	}
	return out, err
}

// Exercise discovery, admission, and exported capabilities through the real
// JobDB HTTP transport. Stored input routes must not hide due job alternates.
func TestAvailableRouteDiscoveryAndAdmission(t *testing.T) {
	for _, mode := range []string{"due alternate", "future alternate", "competing claim", "changed route and demand", "provisioning rejection", "task provisioning rejection"} {
		t.Run(mode, func(t *testing.T) {
			changed := mode == "changed route and demand" || mode == "task provisioning rejection"
			rejected := mode == "provisioning rejection" || mode == "task provisioning rejection"
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			backend := toy.New()
			server := httptest.NewServer(remote.NewServer(backend))
			defer server.Close()
			uri := server.URL + "/tenant"
			client, err := c2j.NewEmbedded([]c2j.Connection{{URI: uri}})
			if err != nil {
				t.Fatal(err)
			}
			handle, err := backend.SubmitJob(ctx, jobdb.SubmitJobRequest{Job: jobdb.SubmitJob{TenantId: "tenant", JobType: "recipe", Data: jobdb.NewTaskDataOrPanic(nil)}})
			if err != nil {
				t.Fatal(err)
			}
			jobRoute := jobdb.Route{JobType: "recipe"}
			inputRoute := jobdb.Route{JobType: "recipe", TaskType: "input:collect_user_input"}
			setup, err := backend.GetJobLease(ctx, jobdb.GetJobLeaseRequest{JobKey: handle.JobKey, WorkerID: "setup", Routes: []jobdb.Route{jobRoute}})
			if err != nil || setup == nil {
				t.Fatal("setup lease", err)
			}
			delay := time.Duration(0)
			if mode == "future alternate" {
				delay = time.Hour
			}
			if err := setup.Reschedule(ctx, jobdb.RescheduleExecutionRequest{
				NextRoute: inputRoute, AlternateRoute: &jobRoute, AlternateAfter: &delay,
				TaskWait: &jobdb.TaskWait{InputOrdinal: 0, OutputOrdinal: 1, InputHash: "input", ResumeJobType: "recipe"},
			}); err != nil {
				t.Fatal(err)
			}

			cfg := &config.Config{Call: time.Second, Batch: time.Second, Lease: time.Minute, Cool: time.Minute, BatchSize: 10, PerTenant: 10, MaxPages: 10, ExecutionTimeout: time.Hour,
				Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: "runner:1", Platform: "linux/arm64"}}
			cfg.Defaults.Routes = []c2j.Route{{JobType: "recipe"}}
			wantRoute := jobRoute
			if changed {
				wantRoute = jobdb.Route{JobType: "recipe", TaskType: "task:other"}
				cfg.Defaults.Routes = append(cfg.Defaults.Routes, c2j.Route{JobType: wantRoute.JobType, TaskType: wantRoute.TaskType})
			}
			cfg.Targets = []config.Target{{Instance: "db", JobDB: uri, Tenant: "tenant", Services: []config.Service{{Name: "p", Priority: 1}}}}
			var listed bool
			list := listFunc(func(ctx context.Context, uri, token string, routes []c2j.Route) (c2j.Page, error) {
				if !reflect.DeepEqual(routes, cfg.Defaults.Routes) {
					t.Error("discovery lost supported routes", routes)
				}
				page, err := client.List(ctx, uri, token, routes)
				if err != nil || len(page.Jobs) == 0 {
					return page, err
				}
				if !listed {
					listed = true
					if page.Jobs[0].Next == nil || page.Jobs[0].Next.TaskType != inputRoute.TaskType {
						t.Error("discovery changed the stored input route", page.Jobs[0].Next)
					}
					if mode == "competing claim" || changed {
						lease, err := backend.GetJobLease(ctx, jobdb.GetJobLeaseRequest{JobKey: handle.JobKey, WorkerID: "other", Routes: []jobdb.Route{jobRoute}})
						if err != nil || lease == nil {
							return c2j.Page{}, fmt.Errorf("competing acquisition failed: %v", err)
						}
						if changed {
							memory := "8Gi"
							demand, err := execution.Initial(nil, "changed", execution.Requirements{Resources: execution.Resources{Memory: &memory}})
							if err != nil {
								return c2j.Page{}, err
							}
							payload, err := execution.PayloadWithDemand(nil, demand)
							if err != nil {
								return c2j.Page{}, err
							}
							revision := lease.ClientPayloadRevision()
							if err := lease.Reschedule(ctx, jobdb.RescheduleExecutionRequest{NextRoute: wantRoute, TaskWait: lease.ExecutionState().TaskWait, ClientPayloadUpdate: &jobdb.ClientPayloadUpdate{Mode: "reset", ExpectedRevision: &revision, Value: payload}}); err != nil {
								return c2j.Page{}, err
							}
						}
					}
				}
				// Duplicate advisory results must still produce only one claim.
				page.Jobs = append(page.Jobs, page.Jobs...)
				return page, nil
			})
			var claims atomic.Int32
			claimer := claimFunc(func(ctx context.Context, uri string, job c2j.Job, worker string, duration time.Duration, routes []c2j.Route) (*c2j.Claimed, error) {
				claims.Add(1)
				claimed, err := client.Claim(ctx, uri, job, worker, duration, routes)
				if claimed != nil && (claimed.Job.Next == nil || *claimed.Job.Next != (c2j.Route{JobType: wantRoute.JobType, TaskType: wantRoute.TaskType})) {
					t.Error("claim retained the stale listed route", claimed.Job.Next)
				}
				return claimed, err
			})
			provider := &routeProvider{reject: rejected}
			controller := Controller{Config: cfg, Lister: list, Claimer: claimer, Scheduler: scheduler.New(time.Minute, time.Second), Providers: map[string]compute.Provider{"p": provider}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			for range 2 {
				if err := controller.Once(ctx); err != nil {
					t.Fatal(err)
				}
			}
			wantClaims, wantLaunches := int32(1), 1
			if mode == "future alternate" {
				wantClaims, wantLaunches = 0, 0
			} else if mode == "competing claim" {
				wantLaunches = 0
			}
			if claims.Load() != wantClaims || len(provider.process) != wantLaunches {
				t.Fatalf("claims=%d launches=%d; want %d/%d", claims.Load(), len(provider.process), wantClaims, wantLaunches)
			}
			key := scheduler.Key{Instance: "db", Tenant: "tenant", Job: handle.JobKey.JobId}
			if rejected {
				if controller.Scheduler.Eligible(key) {
					t.Fatal("provisioning rejection did not retain local cooldown")
				}
				page, err := backend.ListJobs(ctx, jobdb.ListJobsRequest{TenantIds: []string{"tenant"}, JobKeys: []jobdb.JobKey{handle.JobKey}})
				if err != nil || len(page.Jobs) != 1 {
					t.Fatal("inspect released job", err)
				}
				released := page.Jobs[0]
				if released.NextRoute == nil || *released.NextRoute != wantRoute || (released.ExecutionState.TaskWait != nil) != (wantRoute.TaskType != "") {
					t.Fatalf("release lost effective route or task coordinates: %+v", released)
				}
				if lease, err := backend.GetJobLease(ctx, jobdb.GetJobLeaseRequest{JobKey: handle.JobKey, WorkerID: "retry", Routes: []jobdb.Route{wantRoute}}); err != nil || lease != nil {
					t.Fatal("provisioning rejection did not persist backoff", err)
				}
			} else if wantLaunches == 1 {
				capability, err := remote.DecodeLeaseCapability([]byte(provider.process[0].Stdin))
				if err != nil {
					t.Fatal(err)
				}
				runtime, err := remote.New(server.URL, server.Client())
				if err != nil {
					t.Fatal(err)
				}
				lease, err := runtime.ImportLease(ctx, capability)
				if err != nil || lease.Route() != wantRoute {
					t.Fatal("exported lease has incorrect route", err)
				}
				if mode == "changed route and demand" && provider.requests[0].MemoryBytes != 8<<30 {
					t.Fatal("launch did not use acquired payload", provider.requests[0].MemoryBytes)
				}
			}
			if mode == "competing claim" && !controller.Scheduler.Eligible(key) {
				t.Fatal("lost acquisition race incorrectly applied cooldown")
			}
		})
	}
}
