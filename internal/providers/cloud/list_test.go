package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
)

func metadata(id string) map[string]string {
	return map[string]string{"pulse_managed_by": "pulse", "pulse_launch_id": id, "pulse_job_id": "job-" + id}
}
func TestGoogleListActiveAndPagination(t *testing.T) {
	p, _ := New(Config{Kind: "cloudrun", Region: "region", Project: "project"})
	p.accessToken = func(context.Context) (string, error) { return "token", nil }
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v2/projects/project/locations/region/jobs/-/executions" || r.Header.Get("Authorization") != "Bearer token" {
			t.Error(r.Method, r.URL, r.Header)
		}
		row := func(id string) map[string]any {
			return map[string]any{"name": "executions/" + id, "template": map[string]any{"containers": []any{map[string]any{"env": []any{map[string]string{"name": "PULSE_MANAGED_BY", "value": "pulse"}, map[string]string{"name": "PULSE_LAUNCH_ID", "value": id}, map[string]string{"name": "SECRET", "value": "hidden"}}}}}}
		}
		if r.URL.Query().Get("pageToken") == "next" {
			x := row("run")
			x["runningCount"] = 1
			json.NewEncoder(w).Encode(map[string]any{"executions": []any{x}})
			return
		}
		done := row("done")
		done["completionTime"] = "2026-01-01T00:00:00Z"
		failed := row("failed")
		failed["conditions"] = []any{map[string]string{"type": "Completed", "state": "CONDITION_FAILED"}}
		json.NewEncoder(w).Encode(map[string]any{"executions": []any{row("pending"), done, failed}, "nextPageToken": "next"})
	}))
	defer server.Close()
	p.BaseURL = server.URL
	p.HTTP = server.Client()
	out, err := p.List(context.Background(), compute.ListRequest{})
	if err != nil || len(out.Items) != 1 || out.Items[0].State != "starting" || out.NextPageToken != "next" {
		t.Fatal(out, err)
	}
	out, err = p.List(context.Background(), compute.ListRequest{PageToken: out.NextPageToken})
	if err != nil || len(out.Items) != 1 || out.Items[0].State != "running" || calls != 2 {
		t.Fatal(out, err, calls)
	}
}
func TestECSListDescribesTagsAndExcludesStopped(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	p, _ := New(Config{Kind: "ecs", Region: "us-east-1", Cluster: "cluster", ExecutionRole: "role", Subnets: []string{"subnet"}, SupervisorPath: "/helper"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=key/") {
			t.Error("unsigned listing")
		}
		if strings.HasSuffix(r.Header.Get("X-Amz-Target"), "ListTasks") {
			if body["desiredStatus"] != "RUNNING" || body["nextToken"] != "cursor" {
				t.Error(body)
			}
			w.Write([]byte(`{"taskArns":["pending","running","stopped"],"nextToken":"next"}`))
			return
		}
		if strings.HasSuffix(r.Header.Get("X-Amz-Target"), "DescribeTasks") {
			if body["include"].([]any)[0] != "TAGS" {
				t.Error(body)
			}
			rows := []any{}
			for id, state := range map[string]string{"pending": "PENDING", "running": "RUNNING", "stopped": "STOPPED"} {
				tags := []any{}
				for k, v := range metadata(id) {
					tags = append(tags, map[string]string{"key": k, "value": v})
				}
				rows = append(rows, map[string]any{"taskArn": id, "lastStatus": state, "tags": tags})
			}
			json.NewEncoder(w).Encode(map[string]any{"tasks": rows})
			return
		}
		t.Error("unexpected write", r.Header.Get("X-Amz-Target"))
		w.WriteHeader(400)
	}))
	defer server.Close()
	p.BaseURL = server.URL
	p.HTTP = server.Client()
	out, err := p.List(context.Background(), compute.ListRequest{PageToken: "cursor"})
	if err != nil || len(out.Items) != 2 || out.NextPageToken != "next" {
		t.Fatal(out, err)
	}
	for _, row := range out.Items {
		if row.ID == "stopped" {
			t.Fatal(row)
		}
	}
}
func TestAzureNestedPaginationAndTerminalFiltering(t *testing.T) {
	p, _ := New(Config{Kind: "azurejobs", Region: "r", Subscription: "s", ResourceGroup: "g", EnvironmentID: "e"})
	p.accessToken = func(context.Context) (string, error) { return "token", nil }
	root := "/subscriptions/s/resourceGroups/g/providers/Microsoft.App/jobs"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("write from List")
		}
		switch r.URL.Path {
		case root:
			json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": strings.ToLower(root) + "/job", "tags": metadata("launch")}}})
		case root + "/job/executions", strings.ToLower(root) + "/job/executions":
			if r.URL.Query().Get("cursor") == "next" {
				w.Write([]byte(`{"value":[{"id":"execution/second","properties":{"status":"Processing"}}]}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "execution/first", "properties": map[string]string{"status": "Running"}}, map[string]any{"id": "execution/done", "properties": map[string]string{"status": "Succeeded"}}}, "nextLink": root + "/job/executions?cursor=next"})
		default:
			t.Error(r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p.BaseURL = server.URL
	p.HTTP = server.Client()
	q := compute.ListRequest{PageSize: 1}
	ids := []string{}
	for range 4 {
		page, err := p.List(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			ids = append(ids, item.ID)
		}
		if page.NextPageToken == "" {
			break
		}
		q.PageToken = page.NextPageToken
	}
	if strings.Join(ids, ",") != "execution/first,execution/second" {
		t.Fatal(ids)
	}
	// A public cursor cannot turn the authenticated provider into an arbitrary GET proxy.
	for _, path := range []string{"https://elsewhere.example" + root, root + "/job/secrets"} {
		raw, _ := json.Marshal(azureCursor{Scope: root, JobsPage: path})
		_, err := p.List(context.Background(), compute.ListRequest{PageToken: base64.RawURLEncoding.EncodeToString(raw)})
		if err == nil {
			t.Fatal("unsafe cursor accepted", path)
		}
	}
}
func TestListHonorsDeadlineWhileSubmissionOwnsGate(t *testing.T) {
	p, _ := New(Config{Kind: "cloudrun", Project: "p", Region: "r"})
	p.gate <- struct{}{}
	defer p.release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.List(ctx, compute.ListRequest{}); err == nil {
		t.Fatal("expected deadline")
	}
}
