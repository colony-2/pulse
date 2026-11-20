package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	provider "github.com/colony-2/pulse-provider-api"
)

func TestStructuredHTTPProvider(t *testing.T) {
	deadline := time.Date(2030, 1, 2, 3, 4, 5, 123, time.UTC)
	launch := provider.Launch{
		LaunchID: "launch", Lease: "opaque-lease", StartBefore: &deadline, TimeoutSeconds: 30,
		Allocation: provider.Allocation{Base: "ubuntu-*arm64", Capabilities: []string{"git"}, CPUMillis: 1000, MemoryBytes: 2048, ScratchBytes: 512},
		Job:        provider.JobRef{JobDB: "https://db.example/acme", InstanceID: "db", TenantID: "acme", JobID: "job"},
	}
	instance := provider.Instance{ID: "instance", LaunchID: launch.LaunchID, Job: launch.Job, State: "running", Refs: []string{}}
	capabilities := provider.Capabilities{BasesComplete: true, Bases: []provider.BaseCapabilities{{Name: "ubuntu-26.04-arm64", Capabilities: []string{"git"}}}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing bearer token")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /compute/v1/capabilities":
			json.NewEncoder(w).Encode(capabilities)
		case "POST /compute/v1/submit":
			var body struct {
				Items []provider.Launch `json:"items"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !reflect.DeepEqual(body.Items, []provider.Launch{launch}) {
				t.Error("structured assignment changed", err)
			}
			w.Write([]byte(`{"results":[{"launch_id":"launch","status":"accepted"}],"error":"inventory read failed"}`))
		case "GET /compute/v1/launches":
			q := r.URL.Query()
			if q.Get("launch_id") != launch.LaunchID || q.Get("page_token") != "+/=&%" || q.Get("page_size") != "1" {
				t.Error("listing parameters changed", q)
			}
			json.NewEncoder(w).Encode(provider.ListResponse{Items: []provider.Instance{instance}, NextPageToken: "next"})
		default:
			t.Error("unexpected provider request", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p, err := NewStructured(server.URL+"/compute/", server.Client(), func() (string, error) { return "test-token", nil }, false)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := p.GetCapabilities(t.Context()); err != nil || !reflect.DeepEqual(out, capabilities) {
		t.Fatal(out, err)
	}
	if out, err := p.Submit(t.Context(), []provider.Launch{launch}); err == nil || err.Error() != "inventory read failed" || !reflect.DeepEqual(out, []provider.Submission{{LaunchID: launch.LaunchID, Status: provider.Accepted}}) {
		t.Fatal(out, err)
	}
	if out, err := p.List(t.Context(), provider.ListRequest{LaunchID: launch.LaunchID, PageToken: "+/=&%", PageSize: 1}); err != nil || !reflect.DeepEqual(out, provider.ListResponse{Items: []provider.Instance{instance}, NextPageToken: "next"}) {
		t.Fatal(out, err)
	}
}

func TestStructuredHTTPSubmitDoesNotFollowRedirect(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "/other")
		w.WriteHeader(http.StatusTemporaryRedirect)
		w.Write([]byte("credential-in-response"))
	}))
	defer server.Close()
	p, err := NewStructured(server.URL, server.Client(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Submit(t.Context(), []provider.Launch{{
		LaunchID: "launch", Lease: "opaque-lease", Allocation: provider.Allocation{Base: "*"},
		Job: provider.JobRef{JobDB: "https://db.example/acme", InstanceID: "db", TenantID: "acme", JobID: "job"},
	}})
	if len(out) != 0 || err == nil || strings.Contains(err.Error(), "credential-in-response") || calls.Load() != 1 {
		t.Fatal(out, err, calls.Load())
	}
}
