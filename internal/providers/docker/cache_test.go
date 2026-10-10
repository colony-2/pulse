package docker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/colony-2/pulse/pkg/compute"
)

func cachedLaunch(id string) compute.Launch {
	l := launches(requests(id))[0]
	l.Image = "ghcr.io/colony-2/base:latest"
	l.Metadata["pulse_jobdb_instance_id"] = "db"
	l.Metadata["pulse_tenant_id"] = "tenant"
	return l
}

func submitCached(t *testing.T, p *Provider, l compute.Launch) compute.Submission {
	t.Helper()
	out, err := p.Submit(context.Background(), []compute.Launch{l})
	if err != nil || len(out) != 1 {
		t.Fatal(out, err)
	}
	return out[0]
}

func TestAutomaticCacheColdWarmAndRestart(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	if got := submitCached(t, p, cachedLaunch("first")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	if len(d.volumes) != 2 || len(d.creates) != 2 {
		t.Fatal("expected one initializer and two volumes", len(d.creates), d.volumes)
	}
	key := d.containers["first"].Config.Labels[cacheKeyLabel]
	if !verifyCacheContainer(d.containers["first"], key) {
		t.Fatal("missing verified cache mounts")
	}
	if got := submitCached(t, p, cachedLaunch("second")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	if len(d.creates) != 3 || d.containers["second"].Config.Labels[cacheKeyLabel] != key {
		t.Fatal("warm cache reinitialized")
	}
	list, err := p.List(context.Background(), compute.ListRequest{PageSize: 100})
	if err != nil || len(list.Items) != 2 {
		t.Fatal("initializer leaked into job list", list, err)
	}
	d.mu.Lock()
	delete(d.containers, "first")
	delete(d.containers, "second")
	d.mu.Unlock()
	restarted, err := open(context.Background(), p.cfg, p.e, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := submitCached(t, restarted, cachedLaunch("after-restart")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	if len(d.creates) != 4 {
		t.Fatal("restart reinitialized warm cache")
	}
	// A configuration generation change cannot redirect an existing launch.
	restarted.cfg.DependencyCache.Generation = "next"
	if got := submitCached(t, restarted, cachedLaunch("after-restart")); got.Status != compute.Accepted || len(d.creates) != 4 {
		t.Fatal(got)
	}
}

func TestCacheIdentityAndEnvironment(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	base := nativePlan{Request: cachedLaunch("job").Request, ImageID: "image-a"}
	c, env, err := p.cachePlan(base, map[string]string{"NIX_CONFIG": "extra-substituters = https://cache.example\n"})
	if err != nil || c == nil || !strings.Contains(env["NIX_CONFIG"], "extra-substituters") || !strings.HasSuffix(env["NIX_CONFIG"], "min-free = 0\n") {
		t.Fatal(c, env, err)
	}
	for _, change := range []string{"image", "platform", "tenant", "instance", "generation", "namespace"} {
		t.Run(change, func(t *testing.T) {
			plan := base
			plan.Request = cachedLaunch("other-job").Request
			original := p.cfg.DependencyCache
			defer func() { p.cfg.DependencyCache = original }()
			switch change {
			case "image":
				plan.ImageID = "image-b"
			case "platform":
				plan.Request.Platform = "linux/amd64"
			case "tenant":
				plan.Request.Metadata["pulse_tenant_id"] = "other"
			case "instance":
				plan.Request.Metadata["pulse_jobdb_instance_id"] = "other"
			case "generation":
				p.cfg.DependencyCache.Generation = "2"
			case "namespace":
				p.cfg.DependencyCache.Namespace = "other"
			}
			other, _, err := p.cachePlan(plan, nil)
			if err != nil || other.Key == c.Key {
				t.Fatal(other, err)
			}
		})
	}
	for k, v := range map[string]string{"C2J_TOOL_CACHE_DIR": "/elsewhere", "UV_CACHE_DIR": "/elsewhere", "pnpm_config_store_dir": "/elsewhere", "NIX_REMOTE": "daemon", "UV_PYTHON_DOWNLOADS": "automatic", "NIX_CONFIG": "min-free = 10"} {
		if _, _, err := p.cachePlan(base, map[string]string{k: v}); err == nil {
			t.Fatal("accepted conflict", k)
		}
	}
	base.Request.Metadata["pulse_tenant_id"] = ""
	if _, _, err := p.cachePlan(base, nil); err == nil {
		t.Fatal("accepted missing tenant")
	}
}

func TestCacheOptOutAndCustomImages(t *testing.T) {
	for _, custom := range []bool{false, true} {
		d := &daemon{containers: map[string]container{}}
		p, s := testProvider(t, d)
		l := cachedLaunch("job")
		if custom {
			l.Image = "custom:1"
		} else {
			p.cfg.DependencyCache.Disabled = true
		}
		if got := submitCached(t, p, l); got.Status != compute.Accepted || len(d.creates) != 1 || len(d.volumes) != 0 {
			t.Fatal(got)
		}
		s.Close()
	}
}

func TestCacheScratchOverlap(t *testing.T) {
	for _, path := range []string{"/nix", "/nix/store", "/var/cache", "/var/cache/pulse/tools", "/scratch/.."} {
		if !pathsOverlap(path, "/nix") && !pathsOverlap(path, cachePath) {
			t.Errorf("missed cache overlap: %s", path)
		}
	}
	for _, path := range []string{"/scratch", "/nix-other", "/var/cache/pulse-other"} {
		if pathsOverlap(path, "/nix") || pathsOverlap(path, cachePath) {
			t.Errorf("incorrect cache overlap: %s", path)
		}
	}
}

func TestCacheInitializationFailureAndRecovery(t *testing.T) {
	for _, failure := range []string{"volume", "exit", "interrupted-create", "mount"} {
		t.Run(failure, func(t *testing.T) {
			d := &daemon{containers: map[string]container{}}
			p, s := testProvider(t, d)
			defer s.Close()
			switch failure {
			case "volume":
				d.cacheFailure = "POST /volumes/create"
			case "exit":
				d.initExitCode = 1
			case "mount":
				d.dropMount = true
			case "interrupted-create":
				if got := submitCached(t, p, cachedLaunch("first")); got.Status != compute.Accepted {
					t.Fatal(got)
				}
				delete(d.containers, "first")
				for id, c := range d.containers {
					c.State.Status = "created"
					d.containers[id] = c
				}
			}
			got := submitCached(t, p, cachedLaunch("job"))
			if failure == "interrupted-create" {
				if got.Status != compute.Accepted {
					t.Fatal(got)
				}
				return
			}
			want := compute.Unavailable
			if failure == "mount" {
				want = compute.Unknown
			}
			if got.Status != want {
				t.Fatal(got)
			}
			if c, exists := d.containers["job"]; exists && c.State.Status != "created" {
				t.Fatal("started unverified worker")
			}
			delete(d.containers, "job")
			d.cacheFailure = ""
			d.initExitCode = 0
			d.dropMount = false
			if got := submitCached(t, p, cachedLaunch("retry")); got.Status != compute.Accepted {
				t.Fatal(got)
			}
		})
	}
}

func TestCacheOwnershipMismatchDoesNotDeleteVolume(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	if got := submitCached(t, p, cachedLaunch("first")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	delete(d.containers, "first")
	for name, v := range d.volumes {
		v.Labels[cacheOwnerLabel] = "someone-else"
		d.volumes[name] = v
	}
	if got := submitCached(t, p, cachedLaunch("next")); got.Status != compute.Unavailable || len(d.volumes) != 2 {
		t.Fatal(got)
	}
}

func TestCacheRetentionSkipsReferencesAndRecoversPartialRemoval(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	if got := submitCached(t, p, cachedLaunch("live")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	key := d.containers["live"].Config.Labels[cacheKeyLabel]
	for id, c := range d.containers {
		if c.Config.Labels[roleLabel] == cacheInitRole {
			c.State.FinishedAt = time.Now().Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
			d.containers[id] = c
		}
	}
	if err := p.Maintain(context.Background()); err != nil || len(d.volumes) != 2 {
		t.Fatal("retired active cache", err)
	}
	delete(d.containers, "live")
	p.lastMaintenance = time.Time{}
	d.cacheFailure = "DELETE /volumes/pulse-cache-v1-" + key + "-tools"
	if err := p.Maintain(context.Background()); err == nil || len(d.volumes) != 1 {
		t.Fatal("expected partial deletion", err, d.volumes)
	}
	d.cacheFailure = ""
	if got := submitCached(t, p, cachedLaunch("reseed")); got.Status != compute.Accepted || len(d.volumes) != 2 {
		t.Fatal(got)
	}
	delete(d.containers, "reseed")
	for id, c := range d.containers {
		c.State.FinishedAt = time.Now().Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
		d.containers[id] = c
	}
	if err := p.Maintain(context.Background()); err != nil || len(d.volumes) != 0 || len(d.containers) != 0 {
		t.Fatal(err, d.volumes, d.containers)
	}
}

func TestCacheInitializerCancellationRetainsUncertainCharge(t *testing.T) {
	d := &daemon{containers: map[string]container{}, initRunning: true, deleteError: true}
	p, s := testProvider(t, d)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out, _ := p.Submit(ctx, []compute.Launch{cachedLaunch("canceled")})
	if len(out) != 1 || out[0].Status != compute.Unavailable {
		t.Fatal(out)
	}
	used, found, err := p.usage(context.Background())
	if err != nil || used.Slots != 1 || len(found) != 0 {
		t.Fatal("lost initializer reservation", used, found, err)
	}
}

func TestCacheRetentionSkipsUnseenUncertainCreate(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	if got := submitCached(t, p, cachedLaunch("first")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	delete(d.containers, "first")
	d.createError = true
	if got := submitCached(t, p, cachedLaunch("uncertain")); got.Status != compute.Unknown {
		t.Fatal(got)
	}
	p.cfg.DependencyCache.MaxAge = time.Nanosecond
	if err := p.Maintain(context.Background()); err != nil || len(d.volumes) != 2 {
		t.Fatal("evicted cache during uncertain create", err, d.volumes)
	}
}

func TestCacheInitializerUnseenCreateRetainsReservation(t *testing.T) {
	d := &daemon{containers: map[string]container{}, createError: true}
	p, s := testProvider(t, d)
	defer s.Close()
	if got := submitCached(t, p, cachedLaunch("uncertain")); got.Status != compute.Unavailable {
		t.Fatal(got)
	}
	used, _, err := p.usage(context.Background())
	if err != nil || used.Slots != 1 {
		t.Fatal("lost uncertain initializer reservation", used, err)
	}
}

// Exercise both real deadlines: the no-config scheduler budget and the Engine
// Unix-socket transport. The old 30s limits fail before cold copy-up completes.
func TestColdCacheCreationBeyondThirtySeconds(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	// Keep Unix socket paths below macOS's sockaddr_un limit even when its
	// temporary directory already has a long /var/folders prefix.
	dir, err := os.MkdirTemp("", "pulse-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/containers/create") && strings.HasPrefix(r.URL.Query().Get("name"), "pulse-cache-init-") {
			select {
			case <-time.After(31 * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		d.serve(w, r)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	p, err := open(t.Context(), Config{CPUMillis: 2000, MemoryBytes: 4 << 30, MaxContainers: 2}, newEngine(socket), false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte("targets: [{jobdb: 'http://localhost:8080/tenant'}]"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Call >= cfg.Batch || cfg.Batch >= cfg.Lease {
		t.Fatal("defaults must leave time for submission and lease handoff", cfg.Call, cfg.Batch, cfg.Lease)
	}
	ctx, cancel := context.WithTimeout(t.Context(), cfg.Batch)
	defer cancel()
	l := cachedLaunch("slow-copy")
	sched := scheduler.New(cfg.Cool, cfg.Call)
	results, err := sched.Run(ctx, "test", []scheduler.Service{{Name: "docker", Priority: 1, Provider: p}}, []scheduler.Job{{Key: scheduler.Key{Instance: "db", Tenant: "tenant", Job: "slow-copy"}, Request: l.Request, Process: l.Process}})
	if err != nil || len(results) != 1 || results[0].Submission.Status != compute.Accepted {
		t.Fatal(results, err)
	}
}

func TestCacheInitializerCancellationCleansUpAndAllowsRetry(t *testing.T) {
	d := &daemon{containers: map[string]container{}, initRunning: true}
	p, s := testProvider(t, d)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	out, _ := p.Submit(ctx, []compute.Launch{cachedLaunch("canceled")})
	if len(out) != 1 || out[0].Status != compute.Unavailable {
		t.Fatal(out)
	}
	used, _, err := p.usage(t.Context())
	if err != nil || used.Slots != 0 {
		t.Fatal("canceled initializer still occupies capacity", used, err)
	}
	d.mu.Lock()
	d.initRunning = false
	d.mu.Unlock()
	if got := submitCached(t, p, cachedLaunch("retry")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
}
