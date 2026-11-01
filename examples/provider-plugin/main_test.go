package main

import (
	"context"
	"encoding/json"
	"github.com/colony-2/pulse/pkg/compute"
	"github.com/colony-2/pulse/pkg/providerplugin"
	"github.com/colony-2/pulse/pkg/providerplugin/plugintest"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestIndependentExecutable(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "provider")
	if b, e := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); e != nil {
		t.Fatalf("build: %s %v", b, e)
	}
	var mu sync.Mutex
	var launches []compute.Launch
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer selected-token" {
			t.Error("missing credential")
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/health":
			json.NewEncoder(w).Encode(map[string]bool{"ready": true})
		case "/submit":
			var ls []compute.Launch
			if json.NewDecoder(r.Body).Decode(&ls) != nil {
				t.Error("invalid native launch")
			}
			launches = append(launches, ls...)
			out := []compute.Submission{}
			for _, l := range ls {
				out = append(out, compute.Submission{LaunchID: l.LaunchID, Status: compute.Accepted})
			}
			json.NewEncoder(w).Encode(out)
		case "/list":
			var q compute.ListRequest
			json.NewDecoder(r.Body).Decode(&q)
			items := []compute.Instance{}
			for _, l := range launches {
				items = append(items, compute.Instance{ID: l.LaunchID, LaunchID: l.LaunchID, State: "running", Metadata: l.Metadata, Refs: []string{}})
			}
			page, e := compute.Page(items, q)
			if e != nil {
				t.Error(e)
			}
			json.NewEncoder(w).Encode(page)
		default:
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()
	t.Setenv("FIXTURE_TOKEN", "selected-token")
	p, e := providerplugin.Start(context.Background(), providerplugin.ProcessConfig{Command: binary, PassEnv: []string{"FIXTURE_TOKEN"}, Name: "external", BaseDir: t.TempDir(), ConfigYAML: []byte("endpoint: " + backend.URL + "\ntoken_env: FIXTURE_TOKEN\n")})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	l := compute.Launch{Request: compute.Request{LaunchID: "external", Allocation: compute.Allocation{Image: "alpine:3", Platform: "linux/amd64"}, Metadata: map[string]string{"all": "correlation", "unicode": "雪"}}, Process: compute.Process{Command: []string{"run"}, Args: []string{}, Env: map[string]string{"literal": "$HOME"}, Stdin: "private-lease"}}
	plugintest.Check(t, p, l)
	mu.Lock()
	defer mu.Unlock()
	if len(launches) != 1 || !reflect.DeepEqual(launches[0], l) {
		t.Fatal("native launch changed")
	}
	for _, arg := range []string{"version", "--help"} {
		if _, e := exec.Command(binary, arg).CombinedOutput(); e != nil {
			t.Fatal(e)
		}
	}
}
