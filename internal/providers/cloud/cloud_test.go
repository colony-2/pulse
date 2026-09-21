package cloud

import (
	"context"
	"encoding/json"
	"github.com/colony-2/cortex/pkg/compute"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func request(id string) compute.Request {
	return compute.Request{LaunchID: id, Allocation: compute.Allocation{CPUMillis: 1500, MemoryBytes: 3 * Gi, ScratchBytes: Gi, Image: "registry.example/runner@sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64"}, TimeoutSeconds: 60, Metadata: map[string]string{"cortex_job_id": "job", "cortex_launch_id": id}}
}
func prepared(t *testing.T, p *Provider, r compute.Request) compute.PreparedLaunch {
	t.Helper()
	rs, e := p.Prepare(context.Background(), []compute.Request{r})
	if e != nil || rs[0].Status != compute.Prepared {
		t.Fatal(rs, e)
	}
	if e = rs[0].Plan.Allocation.Satisfies(r.Allocation); e != nil {
		t.Fatal(e)
	}
	return compute.PreparedLaunch{LaunchID: r.LaunchID, Plan: *rs[0].Plan, Process: compute.Process{Command: []string{"c2j"}, Args: []string{"run"}, Env: map[string]string{"TEST": "literal $value"}}}
}
func TestCloudRunNativeRequestAndAcceptance(t *testing.T) {
	p, e := New(Config{Kind: "cloudrun", Project: "project", Region: "region"})
	if e != nil {
		t.Fatal(e)
	}
	p.Run = func(context.Context, string, []string, []byte) ([]byte, error) { return []byte("token"), nil }
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
		created = true
		enc.Encode(map[string]any{"done": true})
	}))
	defer s.Close()
	p.BaseURL = s.URL
	p.HTTP = s.Client()
	r, e := p.Submit(context.Background(), []compute.PreparedLaunch{prepared(t, p, request("a"))})
	if e != nil || r[0].Status != compute.Accepted || len(r[0].Refs) != 2 {
		t.Fatal(r, e)
	}
}
func TestGoogleCapacityAndUnknownStart(t *testing.T) {
	for _, code := range []int{429, 500} {
		p, _ := New(Config{Kind: "cloudrun", Project: "p", Region: "r"})
		p.Run = func(context.Context, string, []string, []byte) ([]byte, error) { return []byte("token"), nil }
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, ":run") {
				w.WriteHeader(code)
				return
			}
			w.Write([]byte(`{"done":true}`))
		}))
		p.BaseURL = s.URL
		p.HTTP = s.Client()
		out, _ := p.Submit(context.Background(), []compute.PreparedLaunch{prepared(t, p, request("a"))})
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
		p, _ := New(Config{Kind: "ecs", Region: "r", Cluster: "cluster", Subnets: []string{"subnet"}, ExecutionRole: "role", SupervisorPath: "/usr/local/bin/cortex-exec", ImageStorageBounds: map[string]int64{req.Image: 2 * Gi}})
		calls := 0
		p.Run = func(_ context.Context, cmd string, args []string, _ []byte) ([]byte, error) {
			if cmd != "aws" {
				t.Fatal(cmd)
			}
			var input string
			for i, a := range args {
				if a == "--cli-input-json" {
					input = strings.TrimPrefix(args[i+1], "file://")
				}
			}
			b, e := os.ReadFile(input)
			if e != nil {
				t.Fatal(e)
			}
			var body map[string]any
			json.Unmarshal(b, &body)
			calls++
			if args[1] == "register-task-definition" {
				if body["cpu"] != "2048" || body["memory"] != "4096" {
					t.Fatal(body)
				}
				return []byte(`{"taskDefinition":{"taskDefinitionArn":"arn:def"}}`), nil
			}
			if body["clientToken"] != "ecs" || body["count"] != float64(1) {
				t.Fatal(body)
			}
			if full {
				return []byte(`{"tasks":[],"failures":[{"reason":"RESOURCE:MEMORY"}]}`), nil
			}
			return []byte(`{"tasks":[{"taskArn":"arn:task"}],"failures":[]}`), nil
		}
		out, e := p.Submit(context.Background(), []compute.PreparedLaunch{prepared(t, p, req)})
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
	p.Run = func(context.Context, string, []string, []byte) ([]byte, error) { return []byte("token"), nil }
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
	out, e := p.Submit(context.Background(), []compute.PreparedLaunch{prepared(t, p, req)})
	if e != nil || out[0].Status != compute.Accepted {
		t.Fatal(out, e)
	}
}
func TestUnsupportedScratchEvidence(t *testing.T) {
	p, _ := New(Config{Kind: "ecs", Region: "r", Cluster: "c", Subnets: []string{"s"}, ExecutionRole: "role", SupervisorPath: "helper"})
	out, e := p.Prepare(context.Background(), []compute.Request{request("a")})
	if e != nil || out[0].Status != compute.Unsupported {
		t.Fatal(out, e)
	}
}
