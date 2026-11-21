package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

func TestIndependentExecutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the shared process SDK supports Linux")
	}
	binary := filepath.Join(t.TempDir(), "provider")
	if b, e := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); e != nil {
		t.Fatalf("build: %s %v", b, e)
	}
	var mu sync.Mutex
	var launches []api.Launch
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer selected-token" {
			t.Error("missing credential")
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/health":
			json.NewEncoder(w).Encode(map[string]bool{"ready": true})
		case "/capabilities":
			json.NewEncoder(w).Encode(api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-arm64", Capabilities: []string{"git"}}}})
		case "/submit":
			var ls []api.Launch
			if json.NewDecoder(r.Body).Decode(&ls) != nil {
				t.Error("invalid native launch")
			}
			launches = append(launches, ls...)
			out := []api.Submission{}
			for _, l := range ls {
				out = append(out, api.Submission{LaunchID: l.LaunchID, Status: api.Accepted})
			}
			json.NewEncoder(w).Encode(out)
		case "/list":
			var q api.ListRequest
			json.NewDecoder(r.Body).Decode(&q)
			items := []api.Instance{}
			for _, l := range launches {
				if q.LaunchID == "" || q.LaunchID == l.LaunchID {
					items = append(items, api.Instance{ID: l.LaunchID, LaunchID: l.LaunchID, State: "running", Job: l.Job, Refs: []string{}})
				}
			}
			json.NewEncoder(w).Encode(api.ListResponse{Items: items})
		default:
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()
	ctx := context.Background()
	p, e := plugin.Start(ctx, plugin.StartOptions{
		Executable: binary, Env: map[string]string{"FIXTURE_TOKEN": "selected-token"},
		Init: plugin.Initialization{Name: "external", BaseDir: t.TempDir(), ConfigYAML: []byte("endpoint: " + backend.URL + "\ntoken_env: FIXTURE_TOKEN\n")},
	})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	l := api.Launch{LaunchID: "external", Allocation: api.Allocation{Base: "ubuntu-26*", Capabilities: []string{"git"}}, Job: api.JobRef{JobDB: "https://db.example/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}, Lease: "private-lease"}
	caps, err := p.GetCapabilities(ctx)
	if err != nil || !caps.BasesComplete || len(caps.Bases) != 1 || caps.Bases[0].Name != "ubuntu-26.04-arm64" {
		t.Fatal(caps, err)
	}
	results, _, err := p.Submit(ctx, []api.Launch{l}, api.SubmitOptions{})
	if err != nil || !reflect.DeepEqual(results, []api.Submission{{LaunchID: l.LaunchID, Status: api.Accepted}}) {
		t.Fatal(results, err)
	}
	page, err := p.List(ctx, api.ListRequest{PageSize: 1, LaunchID: l.LaunchID})
	if err != nil || len(page.Items) != 1 || page.Items[0].Job != l.Job {
		t.Fatal(page, err)
	}
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
