package cloud

import (
	"context"
	"encoding/json"
	"github.com/colony-2/pulse/pkg/compute"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func request(id string) compute.Request {
	return compute.Request{LaunchID: id, Allocation: compute.Allocation{CPUMillis: 1500, MemoryBytes: 3 * Gi, ScratchBytes: Gi, Image: "registry.example/runner@sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64"}, TimeoutSeconds: 60, Metadata: map[string]string{"pulse_job_id": "job", "pulse_launch_id": id}}
}
func launch(r compute.Request) compute.Launch {
	return compute.Launch{Request: r, Process: compute.Process{Command: []string{"c2j"}, Args: []string{"run"}, Env: map[string]string{"TEST": "literal $value", "C2J_EXECUTION_CPU": "1500m"}}}
}
func TestCloudRunNativeRequestAndAcceptance(t *testing.T) {
	p, e := New(Config{Kind: "cloudrun", Project: "project", Region: "region"})
	if e != nil {
		t.Fatal(e)
	}
	p.accessToken = func(context.Context) (string, error) { return "token", nil }
	created := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("auth")
		}
		enc := json.NewEncoder(w)
		if strings.HasSuffix(r.URL.Path, ":run") {
			if !created {
				t.Fatal("ran before create")
			}
			enc.Encode(map[string]any{"name": "projects/project/locations/region/operations/run"})
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		task := body["template"].(map[string]any)["template"].(map[string]any)
		if task["maxRetries"] != float64(0) || task["timeout"] != "60s" {
			t.Fatal(task)
		}
		limits := task["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)["limits"].(map[string]any)
		if limits["cpu"] != "2" || limits["memory"] != "4096Mi" {
			t.Fatal(limits)
		}
		container := task["containers"].([]any)[0].(map[string]any)
		found := false
		for _, e := range container["env"].([]any) {
			entry := e.(map[string]any)
			if entry["name"] == "C2J_EXECUTION_CPU" {
				found = entry["value"] == "1500m"
			}
		}
		if !found {
			t.Fatal("rounded CPU changed requested environment", container)
		}
		created = true
		enc.Encode(map[string]any{"done": true})
	}))
	defer s.Close()
	p.BaseURL = s.URL
	p.HTTP = s.Client()
	r, e := p.Submit(context.Background(), []compute.Launch{launch(request("a"))})
	if e != nil || r[0].Status != compute.Accepted {
		t.Fatal(r, e)
	}
}
func TestGoogleCapacityAndUnknownStart(t *testing.T) {
	for _, code := range []int{429, 500} {
		p, _ := New(Config{Kind: "cloudrun", Project: "p", Region: "r"})
		p.accessToken = func(context.Context) (string, error) { return "token", nil }
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ":run") {
				w.WriteHeader(code)
				return
			}
			w.Write([]byte(`{"done":true}`))
		}))
		p.BaseURL = s.URL
		p.HTTP = s.Client()
		out, _ := p.Submit(context.Background(), []compute.Launch{launch(request("a"))})
		want := compute.Unknown
		if code == 429 {
			want = compute.NoCapacity
		}
		if out[0].Status != want {
			t.Fatal(out)
		}
		s.Close()
	}
}
func TestECSNativeTaskAndFailure(t *testing.T) {
	for _, full := range []bool{false, true} {
		req := request("ecs")
		p, _ := New(Config{Kind: "ecs", Region: "r", Cluster: "cluster", Subnets: []string{"subnet"}, ExecutionRole: "role", SupervisorPath: "/usr/local/bin/pulse-exec", ImageStorageBounds: map[string]int64{req.Image: 2 * Gi}})
		calls := 0
		t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
		t.Setenv("AWS_SESSION_TOKEN", "test-session-token")
		t.Setenv("PATH", t.TempDir())
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Authorization"), "Credential=test-access-key/") || r.Header.Get("X-Amz-Security-Token") != "test-session-token" {
				t.Error("missing AWS signing", r.Header)
			}
			var body map[string]any
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				t.Error(e)
				w.WriteHeader(400)
				return
			}
			calls++
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			if strings.HasSuffix(r.Header.Get("X-Amz-Target"), "RegisterTaskDefinition") {
				if body["cpu"] != "2048" || body["memory"] != "4096" {
					t.Error(body)
				}
				w.Write([]byte(`{"taskDefinition":{"taskDefinitionArn":"arn:def"}}`))
				return
			}
			if body["clientToken"] != "ecs" || body["count"] != float64(1) {
				t.Error(body)
			}
			if full {
				w.Write([]byte(`{"tasks":[],"failures":[{"reason":"RESOURCE:MEMORY"}]}`))
				return
			}
			w.Write([]byte(`{"tasks":[{"taskArn":"arn:task"}],"failures":[]}`))
		}))
		defer s.Close()
		p.BaseURL = s.URL
		p.HTTP = s.Client()
		out, e := p.Submit(context.Background(), []compute.Launch{launch(req)})
		want := compute.Accepted
		if full {
			want = compute.NoCapacity
		}
		if e != nil || calls != 2 || out[0].Status != want {
			t.Fatal(out, e, calls)
		}
	}
}
func TestAzureCreateBeforeStart(t *testing.T) {
	req := request("azure")
	p, e := New(Config{Kind: "azurejobs", Region: "r", Subscription: "s", ResourceGroup: "g", EnvironmentID: "environment", ImageStorageBounds: map[string]int64{req.Image: Gi}})
	if e != nil {
		t.Fatal(e)
	}
	p.accessToken = func(context.Context) (string, error) { return "token", nil }
	created := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if !created {
				w.WriteHeader(404)
				return
			}
			w.Write([]byte(`{"properties":{"provisioningState":"Succeeded"}}`))
			return
		}
		if r.Method == "PUT" {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			props := body["properties"].(map[string]any)
			if props["environmentId"] != "environment" {
				t.Fatal(body)
			}
			created = true
			w.Write([]byte(`{}`))
			return
		}
		if !created {
			t.Fatal("start before create")
		}
		w.Write([]byte(`{"id":"/execution"}`))
	}))
	defer s.Close()
	p.BaseURL = s.URL
	p.HTTP = s.Client()
	out, e := p.Submit(context.Background(), []compute.Launch{launch(req)})
	if e != nil || out[0].Status != compute.Accepted {
		t.Fatal(out, e)
	}
}
func TestUnsupportedScratchEvidence(t *testing.T) {
	p, _ := New(Config{Kind: "ecs", Region: "r", Cluster: "c", Subnets: []string{"s"}, ExecutionRole: "role", SupervisorPath: "helper"})
	out, e := p.Submit(context.Background(), []compute.Launch{launch(request("a"))})
	if e != nil || out[0].Status != compute.Unsupported {
		t.Fatal(out, e)
	}
}

func TestEnvironmentPreservesSuppliedValues(t *testing.T) {
	supplied := map[string]string{"PULSE_JOB_ID": "literal", "PULSE_SCRATCH_DIR": "/custom", "TMPDIR": "/custom/tmp", "C2J_EXECUTION_CPU": "1500m"}
	entries := envList(compute.Process{Env: supplied}, map[string]string{"pulse_job_id": "metadata"})
	for _, entry := range entries {
		if want, ok := supplied[entry["name"]]; ok && entry["value"] != want {
			t.Fatal(entry)
		}
	}
}
