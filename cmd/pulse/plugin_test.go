package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	jobremote "github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/toy"
	"github.com/colony-2/pulse/pkg/compute"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestCLIPluginLifecycle(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "pulse")
	plugin := filepath.Join(dir, "provider")
	for _, build := range []struct{ dir, out string }{{".", binary}, {"../../examples/provider-plugin", plugin}} {
		cmd := exec.Command("go", "build", "-o", build.out, ".")
		cmd.Dir = build.dir
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("build %s: %s %v", build.dir, b, e)
		}
	}
	db := httptest.NewServer(jobremote.NewServer(toy.New()))
	defer db.Close()
	var submissions atomic.Int32
	var initializations atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer CANARY-CREDENTIAL" {
			t.Error("missing selected credential")
		}
		switch r.URL.Path {
		case "/health":
			initializations.Add(1)
			json.NewEncoder(w).Encode(map[string]bool{"ready": true})
		case "/list":
			json.NewEncoder(w).Encode(compute.ListResponse{Items: []compute.Instance{}})
		case "/submit":
			submissions.Add(1)
			t.Error("empty JobDB submitted workload")
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	}))
	defer native.Close()
	cfg := map[string]any{"http": map[string]any{"listen": "127.0.0.1:0"}, "defaults": map[string]any{"platform": "linux/amd64"}, "providers": map[string]any{"custom": map[string]any{"type": "plugin", "plugin": map[string]any{"command": plugin, "config": map[string]any{"endpoint": native.URL, "token_file": "token"}}}}, "targets": []any{map[string]any{"instance_id": "test", "jobdb": db.URL + "/tenant", "launch_services": []any{map[string]any{"name": "custom", "priority": 1}}}}}
	data, _ := yaml.Marshal(cfg)
	path := filepath.Join(dir, "pulse.yaml")
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "token"), []byte("CANARY-CREDENTIAL"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, source := range []string{"file", "environment"} {
		for _, args := range [][]string{{"check"}, {"run", "--once"}} {
			t.Run(source+"-"+args[0], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				actual := append([]string{}, args...)
				if source == "file" {
					actual = append(actual, "--config", path)
				}
				cmd := exec.CommandContext(ctx, binary, actual...)
				cmd.Env = []string{"PATH="}
				cmd.Dir = dir
				if source == "environment" {
					cmd.Env = append(cmd.Env, "PULSE_CONFIG="+string(data))
				}
				b, e := cmd.CombinedOutput()
				if e != nil || bytes.Contains(b, []byte("CANARY")) {
					t.Fatalf("CLI failed or leaked configuration: %v %s", e, b)
				}
			})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "run", "--config", path)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH="}
	stderr, e := cmd.StderrPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	address := make(chan string, 1)
	logs := make(chan string, 1)
	go func() {
		var b strings.Builder
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			b.WriteString(line)
			var v struct {
				Address string `json:"address"`
			}
			if json.Unmarshal([]byte(line), &v) == nil && v.Address != "" {
				select {
				case address <- v.Address:
				default:
				}
			}
		}
		logs <- b.String()
	}()
	var addr string
	select {
	case addr = <-address:
	case <-ctx.Done():
		t.Fatal("HTTP API did not start")
	}
	for _, path := range []string{"/config", "/providers/health", "/providers/custom/instances"} {
		resp, e := http.Get("http://" + addr + path)
		if e != nil {
			t.Fatal(e)
		}
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || strings.Contains(b.String(), "CANARY") {
			t.Fatal(path, resp.StatusCode, b.String())
		}
		if path == "/providers/health" && !strings.Contains(b.String(), "ready") {
			t.Fatal("missing plugin health")
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(<-logs, "CANARY") {
		t.Fatal("secret in logs")
	}
	if submissions.Load() != 0 || initializations.Load() != 5 {
		t.Fatal("unexpected native calls", initializations.Load(), submissions.Load())
	}
}
