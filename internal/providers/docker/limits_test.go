package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colony-2/pulse/pkg/compute"
)

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestMissingLimitsPreservesAdmissionAndRestart(t *testing.T) {
	logs := captureWarnings(t)
	d := &daemon{containers: map[string]container{}, noLimits: true}
	p, server := testProvider(t, d)
	defer server.Close()
	if !p.unenforced || strings.Count(logs.String(), "level=WARN") != 1 || !strings.Contains(logs.String(), "without CPU, memory, or swap enforcement") {
		t.Fatalf("missing startup warning: %s", logs)
	}
	results, err := p.Submit(context.Background(), launches(requests("a", "b")))
	if err != nil || len(results) != 2 || results[0].Status != compute.Accepted || results[1].Status != compute.Accepted {
		t.Fatal(results, err)
	}
	for _, c := range d.containers {
		if c.Config.Labels[limitsLabel] != "disabled" || c.HostConfig.NanoCPUs != 0 || c.HostConfig.Memory != 0 || c.HostConfig.MemorySwap != 0 || c.HostConfig.Tmpfs["/scratch"] == "" {
			t.Fatalf("incorrect unenforced container: %+v", c)
		}
	}
	// A daemon gaining enforcement must still account for existing unconstrained
	// containers by their recorded requested allocations.
	d.mu.Lock()
	d.noLimits = false
	d.mu.Unlock()
	restarted, err := open(context.Background(), p.cfg, &engine{base: server.URL, client: server.Client()}, false)
	if err != nil {
		t.Fatal(err)
	}
	results, err = restarted.Submit(context.Background(), launches(requests("next")))
	if err != nil || results[0].Status != compute.NoCapacity {
		t.Fatal(results, err)
	}
	d.mu.Lock()
	for id, c := range d.containers {
		c.State.Status = "exited"
		d.containers[id] = c
	}
	d.mu.Unlock()
	results, err = restarted.Submit(context.Background(), launches(requests("next")))
	if err != nil || results[0].Status != compute.Accepted {
		t.Fatal(results, err)
	}
	if c := d.containers["next"]; c.Config.Labels[limitsLabel] != "enforced" || c.HostConfig.Memory == 0 {
		t.Fatal("new job did not use restored enforcement")
	}
	if strings.Count(logs.String(), "level=WARN") != 1 {
		t.Fatalf("warning repeated during submission: %s", logs)
	}
}

func TestLimitProbe(t *testing.T) {
	for _, tc := range []struct {
		name, limitedError, unlimitedError    string
		dropLimits, wantUnenforced, wantError bool
	}{
		{name: "enforcement available"},
		{name: "threaded cgroup", limitedError: "cannot enter cgroupv2 with domain controllers -- it is in threaded mode", wantUnenforced: true},
		{name: "limits discarded", dropLimits: true, wantUnenforced: true},
		{name: "unrelated startup error", limitedError: "executable file not found", wantError: true},
		{name: "unconstrained containers also fail", limitedError: "cgroup unavailable", unlimitedError: "permission denied", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarnings(t)
			containers := map[string]hostConfig{}
			image := false
			limitedStarts, unlimitedStarts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enc := json.NewEncoder(w)
				switch {
				case r.Method == "POST" && r.URL.Path == "/images/create":
					if r.Header.Get("Content-Type") != "application/x-tar" || r.URL.Query().Get("fromSrc") != "-" {
						t.Error("probe must import local scratch image")
					}
					image = true
					enc.Encode(map[string]string{"status": "imported"})
				case r.Method == "POST" && r.URL.Path == "/containers/create":
					var body struct{ HostConfig hostConfig }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if tc.dropLimits {
						body.HostConfig.Memory = 0
					}
					name := r.URL.Query().Get("name")
					containers[name] = body.HostConfig
					enc.Encode(map[string]string{"Id": name})
				case strings.HasSuffix(r.URL.Path, "/json"):
					name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
					enc.Encode(map[string]any{"HostConfig": containers[name]})
				case strings.HasSuffix(r.URL.Path, "/start"):
					message := tc.unlimitedError
					if strings.HasSuffix(r.URL.Path, "-true/start") {
						limitedStarts++
						message = tc.limitedError
					} else {
						unlimitedStarts++
					}
					if message != "" {
						w.WriteHeader(500)
						enc.Encode(map[string]string{"message": message})
					} else {
						w.WriteHeader(204)
					}
				case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/containers/"):
					delete(containers, strings.TrimPrefix(r.URL.Path, "/containers/"))
					w.WriteHeader(204)
				case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/images/"):
					image = false
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(404)
				}
			}))
			p := &Provider{cfg: Config{Helper: "/helper"}, platform: "linux/arm64", e: &engine{base: server.URL, client: server.Client()}}
			err := p.checkLimits(context.Background())
			server.Close()
			if (err != nil) != tc.wantError || p.unenforced != tc.wantUnenforced {
				t.Fatalf("unenforced=%v error=%v", p.unenforced, err)
			}
			if len(containers) != 0 || image {
				t.Fatal("probe leaked Docker resources")
			}
			if tc.wantUnenforced {
				if unlimitedStarts != 1 || strings.Count(logs.String(), "level=WARN") != 1 {
					t.Fatalf("fallback did not verify startup and warn: starts=%d logs=%s", unlimitedStarts, logs)
				}
			} else if logs.Len() != 0 {
				t.Fatalf("unexpected warning: %s", logs)
			}
			if !tc.dropLimits && limitedStarts != 1 {
				t.Fatal("probe did not try enforced startup")
			}
			if tc.name == "unrelated startup error" && unlimitedStarts != 0 {
				t.Fatal("unrelated error disabled enforcement")
			}
		})
	}
}
