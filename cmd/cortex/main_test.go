package main

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/colony-2/c2j/pkg/joblist"
	"github.com/colony-2/cortex/pkg/compute"
	"github.com/colony-2/jobdb/pkg/jobdb"
	jobremote "github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCLIThroughListingAndRemoteProtocol(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "cortex")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	fixture, err := os.ReadFile("../../internal/c2j/testdata/list-v0.0.52.json")
	if err != nil {
		t.Fatal(err)
	}
	var page joblist.Page
	if err = json.Unmarshal(fixture, &page); err != nil {
		t.Fatal(err)
	}
	job := page.Jobs[0]
	metadata, _ := json.Marshal(map[string]any{"execution": job.Execution.Demand})
	db := httptest.NewServer(jobremote.NewServer(listOnlyRuntime{list: func(context.Context, jobdb.ListJobsRequest) (jobdb.ListJobsResponse, error) {
		return jobdb.ListJobsResponse{Jobs: []jobdb.JobSummary{{JobKey: jobdb.JobKey{TenantId: job.TenantID, JobId: job.JobID}, Status: job.Status, JobType: "recipe", NextRoute: job.NextRoute, Metadata: metadata}}}, nil
	}}))
	defer db.Close()
	for _, source := range []string{"file", "environment"} {
		t.Run(source, func(t *testing.T) {
			var mu sync.Mutex
			submitted := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing auth")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/v1/submit" {
					t.Error(r.URL.Path)
					w.WriteHeader(404)
					return
				}
				var in struct {
					Items []compute.Launch `json:"items"`
				}
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					t.Error(err)
					return
				}
				results := []map[string]any{}
				for _, item := range in.Items {
					if item.CPUMillis != 1000 || item.Image == "" || item.Metadata["cortex_job_id"] == "" || item.Process.Env["C2J_EXECUTION_CPU"] != "1000m" || item.Process.Env["CORTEX_JOB_ID"] == "" || item.Process.Env["CORTEX_CONFIG"] != "" {
						t.Error(item)
					}
					results = append(results, map[string]any{"launch_id": item.LaunchID, "status": "accepted"})
				}
				mu.Lock()
				submitted += len(in.Items)
				mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{"results": results})
			}))
			defer server.Close()
			cfg := map[string]any{
				"defaults":  map[string]any{"image": "registry.example/runner:1", "platform": "linux/arm64", "cpu": "1", "memory": "1Gi", "scratch": "1Gi"},
				"providers": map[string]any{"remote": map[string]any{"type": "remote", "endpoint": server.URL, "allow_http": true, "token_env": "CORTEX_TEST_PROVIDER_TOKEN"}},
				"targets":   []any{map[string]any{"instance_id": "test", "jobdb": db.URL + "/acme", "cells": []string{"github.com/acme/app"}, "launch_services": []any{map[string]any{"name": "remote", "priority": 1}}}},
			}
			data, _ := yaml.Marshal(cfg)
			cmd := exec.Command(binary, "-once")
			cmd.Dir = t.TempDir()
			cmd.Env = []string{"PATH=" + t.TempDir(), "CORTEX_TEST_PROVIDER_TOKEN=test-token"}
			if source == "file" {
				path := filepath.Join(dir, "cortex.yaml")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				cmd.Args = append(cmd.Args, "-config", path)
			} else {
				cmd.Env = append(cmd.Env, "CORTEX_CONFIG="+string(data))
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("CLI: %s %v", out, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if submitted != 1 {
				t.Fatalf("submitted %d", submitted)
			}
		})
	}
}

type listOnlyRuntime struct {
	jobdb.WorkflowRuntime
	list func(context.Context, jobdb.ListJobsRequest) (jobdb.ListJobsResponse, error)
}

func (r listOnlyRuntime) ListJobs(ctx context.Context, req jobdb.ListJobsRequest) (jobdb.ListJobsResponse, error) {
	return r.list(ctx, req)
}

func TestCLIHTTPAndShutdown(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "cortex")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	db := httptest.NewServer(jobremote.NewServer(listOnlyRuntime{list: func(context.Context, jobdb.ListJobsRequest) (jobdb.ListJobsResponse, error) {
		return jobdb.ListJobsResponse{Jobs: []jobdb.JobSummary{}}, nil
	}}))
	defer db.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/launches" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(compute.ListResponse{Items: []compute.Instance{{ID: "queued-one", LaunchID: "launch-one", State: "queued", Metadata: map[string]string{"cortex_launch_id": "launch-one"}, Refs: []string{}}}})
	}))
	defer provider.Close()
	cfg := map[string]any{
		"http":      map[string]any{"listen": "127.0.0.1:0"},
		"defaults":  map[string]any{"image": "registry.example/runner:1", "platform": "linux/amd64", "cpu": "1", "memory": "1Gi", "scratch": "1Gi"},
		"providers": map[string]any{"pool": map[string]any{"type": "remote", "endpoint": provider.URL, "allow_http": true, "token_env": "CORTEX_TEST_PROVIDER_TOKEN"}},
		"targets":   []any{map[string]any{"instance_id": "test", "jobdb": db.URL + "/acme", "cells": []string{"github.com/acme/app"}, "launch_services": []any{map[string]any{"name": "pool", "priority": 1}}}},
	}
	data, _ := yaml.Marshal(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + t.TempDir(), "CORTEX_TEST_PROVIDER_TOKEN=test-token", "CORTEX_CONFIG=" + string(data)}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			var record struct {
				Address string `json:"address"`
			}
			if json.Unmarshal(scanner.Bytes(), &record) == nil && record.Address != "" {
				select {
				case address <- record.Address:
				default:
				}
			}
		}
	}()
	var base string
	select {
	case addr := <-address:
		base = "http://" + addr
	case err := <-done:
		t.Fatalf("controller exited before listening: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, endpoint := range []string{"/status", "/config", "/instances", "/providers/pool/instances", "/scheduler/cooldowns", "/scheduler/round-robin"} {
		res, err := client.Get(base + endpoint) // Public: no Authorization header.
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		err = json.NewDecoder(res.Body).Decode(&body)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %d %v", endpoint, res.StatusCode, err)
		}
		if endpoint == "/instances" && (body["complete"] != true || len(body["items"].([]any)) != 1) {
			t.Fatal(body)
		}
		if endpoint == "/config" && body["http"].(map[string]any)["listen"] != "127.0.0.1:0" {
			t.Fatal("empty config")
		}
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("controller did not shut down")
	}
	if res, err := client.Get(base + "/status"); err == nil {
		res.Body.Close()
		t.Fatal("HTTP listener remained open after shutdown")
	}
}

func TestCLIConfigurationSources(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "cortex")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	data, err := os.ReadFile("../../examples/container.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "cortex.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name      string
		args, env []string
		wantError string
	}{
		{name: "default file", args: []string{"-check"}},
		{name: "explicit file overrides invalid env", args: []string{"-config", "cortex.yaml", "-check"}, env: []string{"CORTEX_CONFIG=[invalid"}},
		{name: "missing explicit file does not fall back", args: []string{"-config", "missing.yaml", "-check"}, env: []string{"CORTEX_CONFIG=" + string(data)}, wantError: "missing.yaml"},
		{name: "invalid env does not fall back", args: []string{"-check"}, env: []string{"CORTEX_CONFIG=[invalid"}, wantError: "CORTEX_CONFIG"},
		{name: "empty env does not fall back", args: []string{"-check"}, env: []string{"CORTEX_CONFIG="}, wantError: "CORTEX_CONFIG"},
		{name: "empty explicit path", args: []string{"-config=", "-check"}, env: []string{"CORTEX_CONFIG=" + string(data)}, wantError: "nonempty file path"},
		{name: "version ignores config", args: []string{"-version"}, env: []string{"CORTEX_CONFIG=[invalid"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(binary, tt.args...)
			cmd.Dir = dir
			cmd.Env = append([]string{"PATH=" + t.TempDir(), "CORTEX_PROVIDER_TOKEN=test-token"}, tt.env...)
			out, err := cmd.CombinedOutput()
			if tt.wantError != "" {
				if err == nil || !strings.Contains(string(out), tt.wantError) {
					t.Fatalf("expected %q failure, got %s %v", tt.wantError, out, err)
				}
			} else if err != nil {
				t.Fatalf("CLI: %s %v", out, err)
			}
		})
	}
}
