package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// Use real compact HTTP discovery and leases, including a yield between the
// two. No detail reads are permitted during the controller pass.
func TestCompactDiscoveryUsesLeasedDemand(t *testing.T) {
	for _, mode := range []string{"changed demand", "absent demand", "unresolved overrides", "no metadata", "malformed demand", "unsupported demand", "malformed metadata", "unsupported metadata", "cancelled after discovery"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			cpu, memory, image := "2", "2Gi", "registry.example/submitted:1"
			initial, err := execution.Initial(nil, mode != "unresolved overrides", execution.Requirements{Resources: execution.Resources{CPU: &cpu, Memory: &memory}, Image: &image})
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := json.Marshal(map[string]any{"execution": initial})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "no metadata":
				metadata = nil
			case "malformed metadata":
				metadata = []byte(`{"execution":"invalid"}`)
			case "unsupported metadata":
				metadata = []byte(`{"execution":{"schema_version":99}}`)
			}
			backend := toy.New()
			handle, err := backend.SubmitJob(ctx, jobdb.SubmitJobRequest{Job: jobdb.SubmitJob{TenantId: "tenant", JobType: "recipe", Metadata: metadata, Data: jobdb.NewTaskDataOrPanic(nil)}})
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			handler := remote.NewServer(backend)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			uri := server.URL + "/tenant"
			client, err := c2j.NewEmbedded([]c2j.Connection{{URI: uri}})
			if err != nil {
				t.Fatal(err)
			}
			var claims atomic.Int32
			list := listFunc(func(ctx context.Context, uri, token string, routes []c2j.Route) (c2j.Page, error) {
				page, err := client.List(ctx, uri, token, routes)
				if err != nil {
					return page, err
				}
				if requests.Load() != 1 || claims.Load() != 0 || len(page.Jobs) != 1 || page.Jobs[0].Execution.Status != "not_loaded" || page.Jobs[0].Execution.Demand != nil {
					t.Error("discovery fetched details, claimed work, or supplied demand")
				}
				if mode == "cancelled after discovery" {
					return page, backend.CancelJob(ctx, jobdb.CancelJobRequest{JobKey: handle.JobKey})
				}
				var payload json.RawMessage
				switch mode {
				case "changed demand":
					cpu, memory, image := "4", "8Gi", "registry.example/leased:2"
					current, err := execution.Initial(nil, true, execution.Requirements{Resources: execution.Resources{CPU: &cpu, Memory: &memory}, Image: &image})
					if err != nil {
						return page, err
					}
					payload, err = execution.PayloadWithDemand(nil, current.Demand)
					if err != nil {
						return page, err
					}
				case "malformed demand":
					payload = json.RawMessage(`{"c2j":{"execution":"invalid"}}`)
				case "unsupported demand":
					payload = json.RawMessage(`{"c2j":{"execution":{"schema_version":99}}}`)
				}
				if payload != nil {
					lease, err := backend.GetJobLease(ctx, jobdb.GetJobLeaseRequest{JobKey: handle.JobKey, WorkerID: "yield", Routes: []jobdb.Route{{JobType: "recipe"}}})
					if err != nil {
						return page, err
					}
					revision := lease.ClientPayloadRevision()
					if err := lease.Reschedule(ctx, jobdb.RescheduleExecutionRequest{NextRoute: lease.Route(), ClientPayloadUpdate: &jobdb.ClientPayloadUpdate{Mode: "reset", ExpectedRevision: &revision, Value: payload}}); err != nil {
						return page, err
					}
				}
				return page, nil
			})
			claimer := claimFunc(func(ctx context.Context, uri string, j c2j.Job, worker string, duration time.Duration, routes []c2j.Route) (*c2j.Claimed, error) {
				claims.Add(1)
				return client.Claim(ctx, uri, j, worker, duration, routes)
			})
			cfg := &config.Config{Call: time.Second, Batch: time.Second, Lease: time.Minute, Cool: time.Minute, BatchSize: 10, PerTenant: 10, MaxPages: 1,
				Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1 << 30, Image: "registry.example/default:1", Platform: "linux/amd64"}}
			cfg.Defaults.Routes = []c2j.Route{{JobType: "recipe"}}
			cfg.Targets = []config.Target{{Instance: "db", JobDB: uri, Tenant: "tenant", Services: []config.Service{{Name: "p", Priority: 1}}}}
			provider := &accept{}
			var logs bytes.Buffer
			c := Controller{Config: cfg, Lister: list, Claimer: claimer, Scheduler: scheduler.New(time.Minute, time.Second), Providers: map[string]compute.Provider{"p": provider}, Log: slog.New(slog.NewTextHandler(&logs, nil))}
			if err := c.Once(ctx); err != nil {
				t.Fatal(err)
			}
			invalid := strings.HasPrefix(mode, "malformed") || strings.HasPrefix(mode, "unsupported")
			cancelled := mode == "cancelled after discovery"
			wantRequests := int32(2) // List and claim; export requires no extra read.
			if invalid {
				wantRequests++ // Release unused lease with persistent backoff.
			}
			if claims.Load() != 1 || requests.Load() != wantRequests {
				t.Fatalf("claims=%d HTTP requests=%d; want 1/%d", claims.Load(), requests.Load(), wantRequests)
			}
			if invalid || cancelled {
				if len(provider.requests) != 0 {
					t.Fatal("invalid or cancelled work reached provider")
				}
				key := scheduler.Key{Instance: "db", Tenant: "tenant", Job: handle.JobKey.JobId}
				if c.Scheduler.Eligible(key) == invalid {
					t.Fatal("incorrect local backoff")
				}
				if invalid {
					if !strings.Contains(logs.String(), "invalid leased execution demand") {
						t.Fatal("preparation error not logged")
					}
					job, err := backend.GetJob(ctx, handle.JobKey)
					if err != nil || job.Status != jobdb.JobStatusAwaitingFuture {
						t.Fatal("unused lease was not released with backoff", err, job.Status)
					}
				}
				return
			}
			if len(provider.requests) != 1 {
				t.Fatal("missing launch")
			}
			wantCPU, wantMemory, wantImage := int64(2000), int64(2<<30), image
			wantCPUEnv, wantMemoryEnv := "2000m", "2097152Ki"
			if mode == "changed demand" {
				wantCPU, wantMemory, wantImage = 4000, 8<<30, "registry.example/leased:2"
				wantCPUEnv, wantMemoryEnv = "4000m", "8388608Ki"
			} else if mode == "no metadata" {
				wantCPU, wantMemory, wantImage = 1000, 1<<30, "registry.example/default:1"
				wantCPUEnv, wantMemoryEnv = "1000m", "1048576Ki"
			}
			request, process := provider.requests[0], provider.process[0]
			if request.CPUMillis != wantCPU || request.MemoryBytes != wantMemory || request.Image != wantImage || process.Env["C2J_EXECUTION_CPU"] != wantCPUEnv || process.Env["C2J_EXECUTION_MEMORY"] != wantMemoryEnv || process.Env["C2J_EXECUTION_IMAGE"] != wantImage {
				t.Fatal("provider request or executor environment did not use loaded demand")
			}
			if process.Stdin == "" || strings.Contains(logs.String(), string(process.Stdin)) {
				t.Fatal("lease capability missing or logged")
			}
		})
	}
}
