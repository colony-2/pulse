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
		createError                           string
		limitedStatus                         int
		dropLimits, wantUnenforced, wantError bool
	}{
		{name: "enforcement available"},
		{name: "threaded cgroup", limitedError: "cannot enter cgroupv2 with domain controllers -- it is in threaded mode", wantUnenforced: true},
		{name: "limits discarded", dropLimits: true, wantUnenforced: true},
		{name: "unrecognized limit rejection", limitedError: "resource configuration not supported by this runtime", wantUnenforced: true},
		{name: "empty daemon error", limitedStatus: 500, wantUnenforced: true},
		{name: "limits rejected at create", createError: "requested resource settings are unsupported", wantUnenforced: true},
		{name: "unrelated startup error", limitedError: "executable file not found", unlimitedError: "executable file not found", wantError: true},
		{name: "desktop mount failure", limitedError: "Mounts denied: /opt/pulse-exec is not shared from the host", unlimitedError: "Mounts denied: /opt/pulse-exec is not shared from the host", wantError: true},
		{name: "authorization failure", limitedError: "access denied", limitedStatus: 403, wantError: true},
		{name: "unconstrained containers also fail", limitedError: "cgroup unavailable", unlimitedError: "permission denied", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarnings(t)
			containers := map[string]hostConfig{}
			limitedStarts, unlimitedStarts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enc := json.NewEncoder(w)
				switch {
				case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/images/"):
					enc.Encode(map[string]string{"Id": "sha256:probe"})
				case r.Method == "POST" && r.URL.Path == "/containers/create":
					var body struct {
						HostConfig hostConfig
						Entrypoint []string
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if len(body.Entrypoint) != 1 || body.Entrypoint[0] != "/bin/true" {
						t.Error("probe must use image executable")
					}
					if body.HostConfig.NanoCPUs != 0 && tc.createError != "" {
						w.WriteHeader(400)
						enc.Encode(map[string]string{"message": tc.createError})
						return
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
					status := 0
					if strings.HasSuffix(r.URL.Path, "-true/start") {
						limitedStarts++
						message = tc.limitedError
						status = tc.limitedStatus
					} else {
						unlimitedStarts++
					}
					if message != "" || status != 0 {
						if status == 0 {
							status = 500
						}
						w.WriteHeader(status)
						enc.Encode(map[string]string{"message": message})
					} else {
						w.WriteHeader(204)
					}
				case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/containers/"):
					delete(containers, strings.TrimPrefix(r.URL.Path, "/containers/"))
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(404)
				}
			}))
			p := &Provider{platform: "linux/arm64", e: &engine{base: server.URL, client: server.Client()}}
			err := p.checkLimits(context.Background())
			server.Close()
			if (err != nil) != tc.wantError || p.unenforced != tc.wantUnenforced {
				t.Fatalf("unenforced=%v error=%v", p.unenforced, err)
			}
			if len(containers) != 0 {
				t.Fatal("probe leaked Docker resources")
			}
			if tc.wantUnenforced {
				if unlimitedStarts != 1 || strings.Count(logs.String(), "level=WARN") != 1 {
					t.Fatalf("fallback did not verify startup and warn: starts=%d logs=%s", unlimitedStarts, logs)
				}
			} else if strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("unexpected warning: %s", logs)
			}
			if !tc.dropLimits && tc.createError == "" && limitedStarts != 1 {
				t.Fatal("probe did not try enforced startup")
			}
			if tc.wantError && !strings.Contains(err.Error(), tc.limitedError) {
				t.Fatalf("lost limited probe daemon details: %v", err)
			}
			if tc.unlimitedError != "" && (unlimitedStarts != 1 || !strings.Contains(err.Error(), tc.unlimitedError) || !strings.Contains(err.Error(), "without resource limits")) {
				t.Fatalf("lost unlimited probe daemon details: starts=%d error=%v", unlimitedStarts, err)
			}
			if tc.limitedStatus == 403 && unlimitedStarts != 0 {
				t.Fatal("retried authorization failure")
			}
		})
	}
}

// Regular job errors must never expose a daemon message that can echo stdin.
func TestProbeDiagnosticsDoNotChangeJobErrorRedaction(t *testing.T) {
	err := &apiError{code: 500, message: "example job secret"}
	if strings.Contains(err.Error(), err.message) {
		t.Fatal("job errors expose sensitive daemon details")
	}
	detail := probeDiagnostic("start container", err)
	if !strings.Contains(detail.Error(), "start container") || !strings.Contains(detail.Error(), err.message) {
		t.Fatal(detail)
	}
}
