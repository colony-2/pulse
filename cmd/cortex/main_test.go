package main

import (
	"encoding/json"
	"github.com/colony-2/cortex/pkg/compute"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCLIThroughExecutableAndRemoteProtocol(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "cortex")
	build := exec.Command("go", "build", "-o", binary, ".")
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %s %v", b, e)
	}
	fixture, err := filepath.Abs("../../internal/c2j/testdata/list-v0.0.52.json")
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "c2j")
	script := `#!/bin/sh
case "$1" in
version) printf 'c2j test\n' ;;
run) printf '%s\n' '--execution-memory' ;;
list) cat "$CORTEX_TEST_FIXTURE" ;;
*) exit 1 ;;
esac
`
	if e := os.WriteFile(stub, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	submitted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing auth")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/prepare" {
			var in struct {
				Items []compute.Request `json:"items"`
			}
			if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
				t.Error(e)
				w.WriteHeader(400)
				return
			}
			out := []compute.Preparation{}
			for _, item := range in.Items {
				a := item.Allocation
				a.CPUMillis = 2000
				out = append(out, compute.Preparation{LaunchID: item.LaunchID, Status: compute.Prepared, Plan: &compute.Plan{Token: "opaque", ExpiresAt: time.Now().Add(time.Hour), Allocation: a}})
			}
			json.NewEncoder(w).Encode(map[string]any{"results": out})
			return
		}
		if r.URL.Path != "/v1/submit" {
			t.Error(r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var in struct {
			Items []struct {
				ID      string          `json:"launch_id"`
				Token   string          `json:"plan_token"`
				Process compute.Process `json:"process"`
			} `json:"items"`
		}
		if e := json.NewDecoder(r.Body).Decode(&in); e != nil {
			t.Error(e)
			return
		}
		results := []map[string]any{}
		for _, item := range in.Items {
			if item.Token != "opaque" || item.Process.Env["C2J_EXECUTION_CPU"] != "2000m" || item.Process.Env["CORTEX_JOB_ID"] == "" {
				t.Error(item)
			}
			results = append(results, map[string]any{"launch_id": item.ID, "status": "accepted", "inspection_uri": "/v1/launches/" + item.ID, "refs": []string{}})
		}
		mu.Lock()
		submitted += len(in.Items)
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer server.Close()
	cfg := map[string]any{"c2j": map[string]any{"executable": stub, "expected_version": "c2j test", "env": map[string]string{"CORTEX_TEST_FIXTURE": fixture}}, "defaults": map[string]any{"image": "registry.example/runner:1", "platform": "linux/arm64", "cpu": "1", "memory": "1Gi", "scratch": "1Gi"}, "providers": map[string]any{"remote": map[string]any{"type": "remote", "endpoint": server.URL, "allow_http": true, "token_env": "CORTEX_TEST_PROVIDER_TOKEN"}}, "targets": []any{map[string]any{"instance_id": "test", "jobdb": "http://jobdb.test/acme", "cells": []string{"/test/cell"}, "launch_services": []any{map[string]any{"name": "remote", "priority": 1}}}}}
	data, _ := yaml.Marshal(cfg)
	configPath := filepath.Join(dir, "cortex.yaml")
	os.WriteFile(configPath, data, 0600)
	cmd := exec.Command(binary, "-config", configPath, "-once")
	cmd.Env = append(os.Environ(), "CORTEX_TEST_PROVIDER_TOKEN=test-token")
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("CLI: %s %v", b, e)
	}
	mu.Lock()
	defer mu.Unlock()
	if submitted != 1 {
		t.Fatalf("submitted %d", submitted)
	}
}
