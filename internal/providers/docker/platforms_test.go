package docker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
)

type probeDaemon struct {
	t                        *testing.T
	mu                       sync.Mutex
	fail                     string
	created, deleted, starts int
	name, platform           string
	imageRef                 string
	base                     *daemon
}

func (d *probeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.49")
	enc := json.NewEncoder(w)
	switch {
	case strings.HasPrefix(path, "/images/") && (strings.Contains(path, "hello-world") || strings.Contains(path, "sha256:probe")):
		if strings.Contains(path, "hello-world") {
			d.imageRef = strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		}
		if d.fail == "resolve" {
			w.WriteHeader(404)
			return
		}
		var platform struct{ OS, Architecture string }
		if r.URL.Query().Has("platform") {
			if err := json.Unmarshal([]byte(r.URL.Query().Get("platform")), &platform); err != nil || platform.OS != "linux" || platform.Architecture == "" {
				d.t.Errorf("probe image inspection must select an OCI platform: %s", r.URL)
			}
		}
		enc.Encode(map[string]string{"Id": "sha256:probe-" + platform.Architecture, "Os": "linux", "Architecture": platform.Architecture})
	case path == "/images/create":
		w.Write([]byte(`{"errorDetail":{"message":"registry unavailable"}}`))
	case path == "/containers/create" && strings.HasPrefix(r.URL.Query().Get("name"), "pulse-platform-probe-"):
		d.created++
		d.name = r.URL.Query().Get("name")
		d.platform = r.URL.Query().Get("platform")
		if d.imageRef != probeImageForPlatform(d.platform) {
			d.t.Error("probe did not use its architecture's manifest", d.imageRef, d.platform)
		}
		var body struct {
			Image      string
			Entrypoint []string
			Cmd        []string
			HostConfig struct {
				NetworkMode string
				Privileged  bool
				Binds       []string
				Mounts      []any
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			d.t.Error(err)
		}
		if body.Image != "sha256:probe-" || !reflect.DeepEqual(body.Entrypoint, []string{"/hello"}) || len(body.Cmd) != 0 || body.HostConfig.NetworkMode != "none" || body.HostConfig.Privileged || len(body.HostConfig.Binds)+len(body.HostConfig.Mounts) != 0 {
			d.t.Errorf("incorrect probe: %+v", body)
		}
		if d.fail == "create" {
			w.WriteHeader(500)
			return
		}
		enc.Encode(map[string]string{"Id": "probe"})
	case path == "/containers/probe/start":
		d.starts++
		if d.fail == "start" || d.fail == "permission" {
			message := "exec format error"
			if d.fail == "permission" {
				message = "access denied"
			}
			w.WriteHeader(500)
			enc.Encode(map[string]string{"message": message})
			return
		}
		w.WriteHeader(204)
	case path == "/containers/probe/wait":
		if r.Method != "POST" || r.URL.Query().Get("condition") != "not-running" {
			d.t.Error("incorrect wait", r.URL)
		}
		switch d.fail {
		case "timeout":
			<-r.Context().Done()
		case "exit":
			enc.Encode(map[string]int{"StatusCode": 1})
		case "missing status":
			w.Write([]byte(`{}`))
		case "wait error":
			w.Write([]byte(`{"StatusCode":0,"Error":{"Message":"daemon failure"}}`))
		default:
			enc.Encode(map[string]int{"StatusCode": 0})
		}
	case r.Method == "DELETE" && strings.HasPrefix(path, "/containers/pulse-platform-probe-"):
		if path != "/containers/"+d.name || r.URL.Query().Get("force") != "1" {
			d.t.Error("incorrect cleanup", r.URL)
		}
		d.deleted++
		w.WriteHeader(204)
	default:
		if d.base != nil {
			d.base.serve(w, r)
			return
		}
		d.t.Errorf("unexpected probe call %s %s", r.Method, r.URL)
		w.WriteHeader(404)
	}
}

func TestPlatformProbe(t *testing.T) {
	for _, tc := range []struct{ fail, status, stage string }{
		{"", "supported", "wait"},
		{"resolve", "inconclusive", "resolve image"},
		{"create", "inconclusive", "create"},
		{"start", "execution_failed", "start"},
		{"permission", "inconclusive", "start"},
		{"exit", "execution_failed", "wait"},
		{"missing status", "inconclusive", "wait"},
		{"wait error", "inconclusive", "wait"},
		{"timeout", "inconclusive", "wait"},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			d := &probeDaemon{t: t, fail: tc.fail}
			server := httptest.NewServer(http.HandlerFunc(d.serve))
			defer server.Close()
			p := &Provider{platform: "linux/arm64", e: &engine{base: server.URL, version: "/v1.49", client: server.Client()}}
			ctx := t.Context()
			if tc.fail == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			got := p.probePlatform(ctx, "linux/amd64")
			if got.Status != tc.status || got.Stage != tc.stage {
				t.Fatalf("%+v", got)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.created != d.deleted {
				t.Fatalf("created=%d deleted=%d", d.created, d.deleted)
			}
		})
	}
}

func TestPlatformDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, native, version, fail string
		want                        []string
	}{
		{"arm daemon", "linux/arm64", "/v1.49", "", []string{"linux/amd64", "linux/arm64"}},
		{"amd daemon", "linux/amd64", "/v1.49", "", []string{"linux/amd64", "linux/arm64"}},
		{"old API", "linux/arm64", "/v1.47", "", []string{"linux/arm64"}},
		{"no emulator", "linux/arm64", "/v1.49", "start", []string{"linux/arm64"}},
		{"offline", "linux/arm64", "/v1.49", "resolve", []string{"linux/arm64"}},
		{"optional timeout", "linux/arm64", "/v1.49", "timeout", []string{"linux/arm64"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &probeDaemon{t: t, fail: tc.fail}
			server := httptest.NewServer(http.HandlerFunc(d.serve))
			defer server.Close()
			p := &Provider{platform: tc.native, e: &engine{base: server.URL, version: tc.version, client: server.Client()}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := p.discoverPlatforms(ctx); err != nil {
				t.Fatal(err)
			}
			if p.NativePlatform() != tc.native || !reflect.DeepEqual(p.SupportedPlatforms(), tc.want) {
				t.Fatal(p.NativePlatform(), p.SupportedPlatforms())
			}
			// Callers cannot mutate the discovered capability set.
			p.SupportedPlatforms()[0] = "invalid"
			if !reflect.DeepEqual(p.SupportedPlatforms(), tc.want) {
				t.Fatal("mutable capability set")
			}
			if len(tc.want) == 1 && !strings.Contains(p.platformReason("linux/amd64"), p.platformProbes["linux/amd64"].Status) {
				t.Fatal("missing discovery diagnostic")
			}
			canceled, stop := context.WithCancel(t.Context())
			stop()
			if err := p.discoverPlatforms(canceled); err != context.Canceled {
				t.Fatal("lost startup cancellation", err)
			}
		})
	}
}

func TestNewDiscoversPlatformsWithoutResourceLimits(t *testing.T) {
	d := &probeDaemon{t: t, base: &daemon{apiVersion: "1.49", multiPlatform: true, noLimits: true, containers: map[string]container{}}}
	// Keep the Unix socket path below macOS's sockaddr_un length limit.
	dir, err := os.MkdirTemp("/tmp", "pulse-platform-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(d.serve)}
	go server.Serve(listener)
	defer server.Close()
	for _, fail := range []string{"", "start", ""} {
		d.mu.Lock()
		d.fail = fail
		d.mu.Unlock()
		p, err := New(t.Context(), Config{Socket: "unix://" + socket, LockDir: t.TempDir(), DependencyCache: CacheConfig{Disabled: true}})
		if err != nil {
			t.Fatal(err)
		}
		if !p.unenforced || p.supportsPlatform("linux/amd64") != (fail == "") {
			t.Fatal(p.SupportedPlatforms())
		}
		p.Close()
	}
}

func TestMixedPlatformAdmissionAndCacheInitialization(t *testing.T) {
	d := &daemon{apiVersion: "1.49", multiPlatform: true, containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	p.platformProbes = map[string]platformProbe{"linux/amd64": {Status: "supported"}}
	jobs := []compute.Launch{cachedLaunch("arm"), cachedLaunch("amd")}
	jobs[1].Platform = "linux/amd64"
	// Use the same tag for both architectures, preserving separate cache volumes.
	for _, job := range jobs {
		out, err := p.Submit(t.Context(), []compute.Launch{job})
		if err != nil || out[0].Status != compute.Accepted {
			t.Fatal(out, err)
		}
	}
	if len(d.volumes) != 4 {
		t.Fatal("architectures shared cache volumes", len(d.volumes))
	}
	expectedPlatforms := map[string]string{
		d.containers["arm"].Config.Labels[cacheKeyLabel]: "linux/arm64",
		d.containers["amd"].Config.Labels[cacheKeyLabel]: "linux/amd64",
	}
	for i, body := range d.creates {
		platform := d.createPlatforms[i]
		key, _ := body["Labels"].(map[string]any)[cacheKeyLabel].(string)
		if body["Image"] != "sha256:config" || expectedPlatforms[key] != platform {
			t.Fatal("wrong image for worker or initializer", body, platform)
		}
	}
	// Restart accounting must include both architectures in the same budget.
	restarted, err := open(t.Context(), p.cfg, &engine{base: server.URL, client: server.Client()}, false)
	if err != nil {
		t.Fatal(err)
	}
	restarted.platformProbes = p.platformProbes
	third := cachedLaunch("next")
	third.Platform = "linux/amd64"
	out, err := restarted.Submit(t.Context(), []compute.Launch{third})
	if err != nil || out[0].Status != compute.NoCapacity {
		t.Fatal(out, err)
	}
	// Free only the arm worker and select its variant again after the amd pull.
	d.mu.Lock()
	c := d.containers["arm"]
	c.State.Status = "exited"
	d.containers["arm"] = c
	d.mu.Unlock()
	third.Platform = "linux/arm64"
	out, err = restarted.Submit(t.Context(), []compute.Launch{third})
	if err != nil || out[0].Status != compute.Accepted {
		t.Fatal(out, err)
	}
}

func TestPlatformAndDigestDeclinesDoNotCreateWorkers(t *testing.T) {
	d := &daemon{apiVersion: "1.49", multiPlatform: true, containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	job := launches(requests("disabled"))[0]
	job.Platform = "linux/amd64"
	out, err := p.Submit(t.Context(), []compute.Launch{job})
	if err != nil || out[0].Status != compute.Unsupported || len(d.creates) != 0 {
		t.Fatal(out, err)
	}
	p.platformProbes = map[string]platformProbe{"linux/amd64": {Status: "supported"}}
	job.Image = "registry.example/runner@sha256:" + strings.Repeat("a", 64)
	out, err = p.Submit(t.Context(), []compute.Launch{job})
	if err != nil || out[0].Status != compute.Unsupported || len(d.creates) != 0 {
		t.Fatal(out, err)
	}
}
