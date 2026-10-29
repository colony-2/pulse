package c2j

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/c2j/pkg/joblist"
	"github.com/colony-2/jobdb/pkg/jobdb"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/toy"
	"github.com/colony-2/pulse/pkg/compute"
)

type readOnly struct {
	*toy.Runtime
	list func(context.Context, jobdb.ListJobsRequest) (jobdb.ListJobsResponse, error)
}

func (r readOnly) ListJobs(ctx context.Context, req jobdb.ListJobsRequest) (jobdb.ListJobsResponse, error) {
	return r.list(ctx, req)
}

func TestEmbeddedProjectionPaginationAndIsolation(t *testing.T) {
	memory := "8Gi"
	demand, err := execution.Initial(nil, true, execution.Requirements{Resources: execution.Resources{Memory: &memory}})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(map[string]any{"execution": demand, "repo": "https://github.com/acme/app.git"})
	if err != nil {
		t.Fatal(err)
	}
	currentMemory := "16Gi"
	current, err := execution.Initial(nil, true, execution.Requirements{Resources: execution.Resources{Memory: &currentMemory}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := execution.PayloadWithDemand(nil, current.Demand)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	handler := remote.NewServer(readOnly{Runtime: toy.New(), list: func(_ context.Context, req jobdb.ListJobsRequest) (jobdb.ListJobsResponse, error) {
		if req.MetadataFilter != nil {
			t.Error("tenant discovery must not filter repositories")
		}
		if !reflect.DeepEqual(req.AvailableRoutes, []jobdb.Route{{JobType: "recipe"}, {JobType: "recipe", TaskType: "task:custom"}}) || len(req.JobTypes) != 0 || len(req.Statuses) != 0 || req.PageSize != 100 || len(req.TenantIds) != 1 {
			t.Errorf("incorrect listing query: %+v", req)
		}
		// Empty pages can still have a continuation token.
		if req.PageToken == "" {
			return jobdb.ListJobsResponse{NextPageToken: "opaque-next"}, nil
		}
		if req.PageToken != "opaque-next" {
			t.Errorf("changed opaque token %q", req.PageToken)
		}
		return jobdb.ListJobsResponse{Jobs: []jobdb.JobSummary{{
			JobKey: jobdb.JobKey{TenantId: req.TenantIds[0], JobId: "job"}, GoalID: "goal", OtherGoalIDs: []string{}, JobType: "recipe", Status: jobdb.JobStatusCrashConcern,
			NextRoute: &jobdb.Route{JobType: "recipe", TaskType: "input:opaque"}, AvailableAt: now, CancelRequested: true,
			Metadata: metadata, ClientPayload: payload,
		}, {JobKey: jobdb.JobKey{TenantId: req.TenantIds[0], JobId: "bad"}, GoalID: "bad-goal", OtherGoalIDs: []string{}, Status: jobdb.JobStatusReady, Metadata: json.RawMessage(`{"execution":{"schema_version":99}}`)}}}, nil
	}})
	t.Setenv("PULSE_TEST_DB_TOKEN", "db-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer db-token" {
			t.Error("missing JobDB authorization")
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	c, err := NewEmbedded([]Connection{{server.URL + "/one", "PULSE_TEST_DB_TOKEN"}, {server.URL + "/two", "PULSE_TEST_DB_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	routes := []Route{{JobType: "recipe"}, {JobType: "recipe", TaskType: "task:custom"}}
	var wg sync.WaitGroup
	for _, tenant := range []string{"one", "two"} {
		wg.Go(func() {
			first, err := c.List(context.Background(), server.URL+"/"+tenant, "", routes)
			if err != nil || len(first.Jobs) != 0 || first.Next != "opaque-next" {
				t.Error(first, err)
				return
			}
			page, err := c.List(context.Background(), server.URL+"/"+tenant, first.Next, routes)
			if err != nil || len(page.Jobs) != 2 {
				t.Error(page, err)
				return
			}
			j := page.Jobs[0]
			if j.Tenant != tenant || j.Repository != "https://github.com/acme/app.git" || j.Next.TaskType != "input:opaque" || !j.AvailableAt.Equal(now) || !j.CancelRequested || j.Execution.Status != "specified" || j.Execution.Source != "yield" || !reflect.DeepEqual(j.Execution.Demand, &current.Demand) || !reflect.DeepEqual(j.Execution.Initial, &demand) || !reflect.DeepEqual(execution.Inspect(j.Metadata, nil).Initial, &demand) {
				t.Errorf("lost typed data: %+v", j)
			}
			a, err := j.Allocation(compute.Allocation{CPUMillis: 1000, MemoryBytes: 1024, ScratchBytes: 1024, Platform: "linux/amd64", Image: "alpine:3"})
			if err != nil || a.MemoryBytes != 16<<30 {
				t.Error("list projection lost stored demand", a, err)
			}
			if page.Jobs[1].Execution.Status != "unsupported" || page.Jobs[1].Execution.Diagnostic == "" {
				t.Error(page.Jobs[1].Execution)
			}
			if _, err = page.Jobs[1].Allocation(a); err == nil {
				t.Error("invalid demand defaulted")
			}
		})
	}
	wg.Wait()
}

func TestEmbeddedCancellationAndRedirect(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	c, err := NewEmbedded([]Connection{{URI: server.URL + "/tenant"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.List(ctx, server.URL+"/tenant", "", []Route{{JobType: "recipe"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(release)
	server.Close()
	t.Setenv("PULSE_TEST_DB_TOKEN", "secret")
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect") }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	c, err = NewEmbedded([]Connection{{redirect.URL + "/tenant", "PULSE_TEST_DB_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.List(context.Background(), redirect.URL+"/tenant", "", []Route{{JobType: "recipe"}}); err == nil {
		t.Fatal("redirect accepted")
	}
}

func TestEmbeddedInputValidation(t *testing.T) {
	client, err := NewEmbedded([]Connection{{URI: "https://example.com/t"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(t.Context(), "https://example.com/t", "", nil); err == nil {
		t.Fatal("empty routes allowed unfiltered discovery")
	}
	if _, err := client.Claim(t.Context(), "https://example.com/t", Job{}, "worker", time.Minute, nil); err == nil {
		t.Fatal("empty claim routes accepted")
	}
	if _, err := NewEmbedded([]Connection{{URI: "https://example.com"}}); !errors.Is(err, joblist.ErrInvalidInput) {
		t.Fatal(err)
	}
	t.Setenv("PULSE_TEST_EMPTY_TOKEN", "")
	if _, err := NewEmbedded([]Connection{{"https://example.com/t", "PULSE_TEST_EMPTY_TOKEN"}}); err == nil {
		t.Fatal("missing token ignored")
	}
	if _, err := NewEmbedded([]Connection{{URI: "https://example.com/t"}, {"https://example.com/t", "OTHER"}}); err == nil {
		t.Fatal("conflicting auth ignored")
	}
}

func TestDiscoveryMakesOneRequest(t *testing.T) {
	requests := 0
	handler := remote.NewServer(readOnly{Runtime: toy.New(), list: func(_ context.Context, req jobdb.ListJobsRequest) (jobdb.ListJobsResponse, error) {
		jobs := make([]jobdb.JobSummary, 100)
		for i := range jobs {
			jobs[i] = jobdb.JobSummary{JobKey: jobdb.JobKey{TenantId: "tenant", JobId: fmt.Sprint(i)}, GoalID: "goal", Status: jobdb.JobStatusReady}
		}
		return jobdb.ListJobsResponse{Jobs: jobs, NextPageToken: "next"}, nil
	}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := NewEmbedded([]Connection{{URI: server.URL + "/tenant"}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.List(t.Context(), server.URL+"/tenant", "", []Route{{JobType: "recipe"}})
	if err != nil || len(page.Jobs) != 100 || page.Next != "next" || requests != 1 {
		t.Fatalf("jobs=%d requests=%d next=%q error=%v", len(page.Jobs), requests, page.Next, err)
	}
	for _, job := range page.Jobs {
		if job.Execution.Status != "unresolved" || job.Execution.Source != "absent" || job.Execution.Demand != nil {
			t.Fatal("discovery fabricated current demand")
		}
	}
}
