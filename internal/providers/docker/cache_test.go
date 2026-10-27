package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	l.Image = "ghcr.io/colony-2/base2:latest"
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
	// Every attachment must disable daemon-side copying, which can outlive
	// a timed-out create before the helper becomes inspectable/removable.
	for _, create := range d.creates {
		mounts := create["HostConfig"].(map[string]any)["Mounts"].([]any)
		for _, raw := range mounts {
			mount := raw.(map[string]any)
			if mount["VolumeOptions"].(map[string]any)["NoCopy"] != true {
				t.Fatal("unbounded Docker volume copy-up enabled", mount)
			}
		}
	}
	key := d.containers["first"].Config.Labels[cacheKeyLabel]
	if _, exists := d.containers["cache-init-"+key]; exists {
		t.Fatal("initializer retained after completion")
	}
	if !d.readyVolumes[(dependencyCache{Key: key}).volume("nix")] {
		t.Fatal("ready marker not written")
	}
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
	if len(d.creates) != 5 || d.helperScripts["cache-init-"+key] != cacheCheckScript {
		t.Fatal("restart did not check ready marker")
	}
	// A configuration generation change cannot redirect an existing launch.
	restarted.cfg.DependencyCache.Generation = "next"
	if got := submitCached(t, restarted, cachedLaunch("after-restart")); got.Status != compute.Accepted || len(d.creates) != 5 {
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
			if err != nil || (other.Key == c.Key) != (change == "image") {
				t.Fatal(other, err)
			}
		})
	}
	for k, v := range map[string]string{"C2J_TOOL_CACHE_DIR": "/elsewhere", "UV_CACHE_DIR": "/elsewhere", "NIX_CACHE_HOME": "/elsewhere", "pnpm_config_store_dir": "/elsewhere", "NIX_REMOTE": "daemon", "UV_PYTHON_DOWNLOADS": "automatic", "NIX_CONFIG": "min-free = 10"} {
		if _, _, err := p.cachePlan(base, map[string]string{k: v}); err == nil {
			t.Fatal("accepted conflict", k)
		}
	}
	base.Request.Metadata["pulse_tenant_id"] = ""
	if _, _, err := p.cachePlan(base, nil); err == nil {
		t.Fatal("accepted missing tenant")
	}
}

func TestCacheSharesNixAcrossImages(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	if got := submitCached(t, p, cachedLaunch("first")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	d.imageID = "sha256:other"
	if got := submitCached(t, p, cachedLaunch("second")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	first, second := d.containers["first"], d.containers["second"]
	if first.Config.Labels[cacheKeyLabel] != second.Config.Labels[cacheKeyLabel] || len(d.volumes) != 2 || len(d.creates) != 3 {
		t.Fatal("image change recreated shared store")
	}
	a, b := envMap(first.Config.Env), envMap(second.Config.Env)
	for _, key := range []string{"C2J_TOOL_CACHE_DIR", "UV_CACHE_DIR", "pnpm_config_store_dir", "NIX_CACHE_HOME"} {
		if a[key] == b[key] {
			t.Fatalf("images share prepared environment: %s", key)
		}
	}
	if !verifyCacheContainer(second, second.Config.Labels[cacheKeyLabel]) {
		t.Fatal("second image cache attachment invalid")
	}
}

func TestNixFetchCacheEnvironmentAndFreshness(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	plan := nativePlan{Request: cachedLaunch("job").Request, ImageID: "image-a"}
	for _, ttl := range []string{"", "tarball-ttl = 0", "tarball-ttl = 3600", "tarball-ttl = 12"} {
		_, env, err := p.cachePlan(plan, map[string]string{"NIX_CONFIG": ttl, "HOME": "/private", "XDG_CACHE_HOME": "/private/cache"})
		if err != nil {
			t.Fatal(err)
		}
		if env["NIX_CACHE_HOME"] != cachePath+"/images/"+cacheHash(plan.ImageID)+"/nix" || env["HOME"] != "" || env["XDG_CACHE_HOME"] != "" {
			t.Fatal("Nix cache redirected unrelated state", env)
		}
		if env["NIX_CONFIG"] != ttl+"\nmin-free = 0\n" {
			t.Fatal("changed freshness policy", env)
		}
	}
	plan.ImageEnv = []string{"NIX_CACHE_HOME=/private", "NIX_CONFIG=tarball-ttl = 12"}
	if _, _, err := p.cachePlan(plan, nil); err == nil {
		t.Fatal("accepted image cache conflict")
	}
	plan.ImageEnv = []string{"NIX_CONFIG=tarball-ttl = 12"}
	if _, env, err := p.cachePlan(plan, nil); err != nil || env["NIX_CONFIG"] != "tarball-ttl = 12\nmin-free = 0\n" {
		t.Fatal(env, err)
	}
}

func TestCacheWorkersBeforeFetchSharingRemainInspectable(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	if got := submitCached(t, p, cachedLaunch("old")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	c := d.containers["old"]
	var env []string
	for _, entry := range c.Config.Env {
		if !strings.HasPrefix(entry, "NIX_CACHE_HOME=") {
			env = append(env, entry)
		}
	}
	c.Config.Env = env
	if !verifyCacheContainer(c, c.Config.Labels[cacheKeyLabel]) {
		t.Fatal("old v2 worker rejected")
	}
	c.Config.Env = append(c.Config.Env, "NIX_CACHE_HOME=/wrong")
	if verifyCacheContainer(c, c.Config.Labels[cacheKeyLabel]) {
		t.Fatal("incorrect fetch cache accepted")
	}
}

func TestCacheRestartWithMissingMarkerRecreatesPair(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	if got := submitCached(t, p, cachedLaunch("first")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	before := map[string]string{}
	for name, v := range d.volumes {
		before[name] = v.CreatedAt
	}
	delete(d.containers, "first")
	clear(d.readyVolumes) // A failed/interrupted initialization never publishes readiness.
	restarted, err := open(t.Context(), p.cfg, p.e, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := submitCached(t, restarted, cachedLaunch("retry")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	for name, v := range d.volumes {
		if v.CreatedAt == before[name] {
			t.Fatal("unready volume not replaced", name)
		}
	}
	if len(d.creates) != 5 {
		t.Fatal("expected check, replacement initializer, worker", len(d.creates))
	}
}

func TestCacheReadinessCheckFailurePreservesStore(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	if got := submitCached(t, p, cachedLaunch("first")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	delete(d.containers, "first")
	before := map[string]string{}
	for name, v := range d.volumes {
		before[name] = v.CreatedAt
	}
	restarted, err := open(t.Context(), p.cfg, p.e, false)
	if err != nil {
		t.Fatal(err)
	}
	d.initExitCode = 1
	if got := submitCached(t, restarted, cachedLaunch("retry")); got.Status != compute.Unavailable {
		t.Fatal(got)
	}
	for name, created := range before {
		if d.volumes[name].CreatedAt != created {
			t.Fatal("check failure destroyed store")
		}
	}
}

func TestCacheOptOutAndCustomImages(t *testing.T) {
	for _, image := range []string{"", "custom:1", "ghcr.io/colony-2/base:latest"} {
		d := &daemon{containers: map[string]container{}}
		p, s := testProvider(t, d)
		l := cachedLaunch("job")
		if image != "" {
			l.Image = image
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
	for _, failure := range []string{"volume", "exit", "mount"} {
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

			}
			got := submitCached(t, p, cachedLaunch("job"))
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

func TestCacheMissingMarkerDoesNotDeleteActiveStore(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	if got := submitCached(t, p, cachedLaunch("active")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	clear(d.readyVolumes)
	restarted, err := open(t.Context(), p.cfg, p.e, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := submitCached(t, restarted, cachedLaunch("next")); got.Status != compute.Unavailable || len(d.volumes) != 2 {
		t.Fatal(got)
	}
	if d.containers["active"].State.Status != "running" {
		t.Fatal("active worker removed")
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
	for name, v := range d.volumes {
		v.CreatedAt = time.Now().Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
		d.volumes[name] = v
	}
	if err := p.Maintain(context.Background()); err != nil || len(d.volumes) != 2 {
		t.Fatal("retired active cache", err)
	}
	delete(d.containers, "live")
	p.lastMaintenance = time.Time{}
	d.cacheFailure = "DELETE /volumes/pulse-cache-v2-" + key + "-tools"
	if err := p.Maintain(context.Background()); err == nil || len(d.volumes) != 1 {
		t.Fatal("expected partial deletion", err, d.volumes)
	}
	d.cacheFailure = ""
	if got := submitCached(t, p, cachedLaunch("reseed")); got.Status != compute.Accepted || len(d.volumes) != 2 {
		t.Fatal(got)
	}
	delete(d.containers, "reseed")
	for name, v := range d.volumes {
		v.CreatedAt = time.Now().Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
		d.volumes[name] = v
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
// Unix-socket transport. Even a slow create must use the submission deadline.
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

// Exercise the actual shell scripts as well as the fake Docker API: a failed
// init must not publish readiness, and checking readiness must not call Nix.
func TestCacheReadyFileScripts(t *testing.T) {
	dir := t.TempDir()
	nix := filepath.Join(dir, "nix-store")
	marker := filepath.Join(dir, ".pulse-ready")
	run := func(script string) error {
		cmd := exec.Command("/bin/sh", "-ec", strings.ReplaceAll(script, cacheReadyFile, `"`+marker+`"`))
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "C2J_TOOL_CACHE_DIR="+dir+"/tools", "UV_CACHE_DIR="+dir+"/uv", "pnpm_config_store_dir="+dir+"/pnpm", "NIX_CACHE_HOME="+dir+"/nix-cache")
		out, err := cmd.CombinedOutput()
		if len(out) != 0 {
			t.Logf("helper: %s", out)
		}
		return err
	}
	if err := os.WriteFile(nix, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := run(cacheInitializeScript); err == nil {
		t.Fatal("failed Nix init succeeded")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("failed init published ready marker", err)
	}
	if err := run(cacheCheckScript); err == nil || err.(*exec.ExitError).ExitCode() != cacheNotReady {
		t.Fatal("missing marker not reported", err)
	}
	if err := os.WriteFile(nix, []byte("#!/bin/sh\n[ \"$1\" = --init ]\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := run(cacheInitializeScript); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "nix-cache"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("fetch cache directory must be private to the runtime user", info, err)
	}
	if err := os.Remove(nix); err != nil {
		t.Fatal(err)
	}
	if err := run(cacheCheckScript); err != nil {
		t.Fatal("warm readiness invoked Nix", err)
	}
}

func TestCacheRestartRecoversRunningInitializerInSingleSlotPool(t *testing.T) {
	d := &daemon{containers: map[string]container{}, initRunning: true, deleteError: true}
	p, server := testProvider(t, d)
	defer server.Close()
	p.cfg.MaxContainers = 1
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	out, err := p.Submit(ctx, []compute.Launch{cachedLaunch("interrupted")})
	if (err != nil && !errors.Is(err, context.DeadlineExceeded)) || len(out) != 1 || out[0].Status != compute.Unavailable || len(d.containers) != 1 {
		t.Fatal(out, err)
	}
	d.mu.Lock()
	d.deleteError = false
	d.initRunning = false
	d.mu.Unlock()
	restarted, err := open(t.Context(), p.cfg, p.e, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := submitCached(t, restarted, cachedLaunch("recovered")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	if len(d.containers) != 1 {
		t.Fatal("orphan helper survived", d.containers)
	}
}

func TestCacheMissingStoreClearsPreparedRecords(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	if got := submitCached(t, p, cachedLaunch("first")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	c := dependencyCache{Key: d.containers["first"].Config.Labels[cacheKeyLabel]}
	toolsCreated := d.volumes[c.volume("tools")].CreatedAt
	delete(d.containers, "first")
	delete(d.volumes, c.volume("nix"))
	delete(d.readyVolumes, c.volume("nix"))
	restarted, err := open(t.Context(), p.cfg, p.e, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := submitCached(t, restarted, cachedLaunch("recovered")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	if d.volumes[c.volume("tools")].CreatedAt == toolsCreated {
		t.Fatal("old prepared records survived store replacement")
	}
}

func TestCacheConcurrentSubmissionsInitializeOnce(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	results := make(chan error, 2)
	for _, id := range []string{"first", "second"} {
		go func() {
			out, err := p.Submit(t.Context(), []compute.Launch{cachedLaunch(id)})
			if err == nil && (len(out) != 1 || out[0].Status != compute.Accepted) {
				err = fmt.Errorf("submit: %v", out)
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(d.creates) != 3 || len(d.volumes) != 2 {
		t.Fatal("concurrent submissions initialized more than once")
	}
}

func TestCacheLegacyResourcesRemainInspectable(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, server := testProvider(t, d)
	defer server.Close()
	if got := submitCached(t, p, cachedLaunch("legacy")); got.Status != compute.Accepted {
		t.Fatal(got)
	}
	worker := d.containers["legacy"]
	key := worker.Config.Labels[cacheKeyLabel]
	delete(worker.Config.Labels, cacheLayoutLabel) // v1 workers did not carry a layout label.
	worker.Config.Env = []string{"NIX_CONFIG=\nmin-free = 0\n"}
	for k, v := range cacheEnvironment("") {
		worker.Config.Env = append(worker.Config.Env, k+"="+v)
	}
	for i := range worker.Mounts {
		worker.Mounts[i].Name = strings.Replace(worker.Mounts[i].Name, "pulse-cache-v2-", "pulse-cache-v1-", 1)
	}
	if !verifyCacheContainer(worker, key) {
		t.Fatal("legacy worker no longer inspectable")
	}
	d.containers["legacy"] = worker
	helper := worker
	helper.Config.Labels = map[string]string{cacheKeyLabel: key, cacheOwnerLabel: "old-owner", cacheLayoutLabel: "1", roleLabel: cacheInitRole, "pulse_launch_id": "cache-init-" + key, accountingLabel: worker.Config.Labels[accountingLabel], limitsLabel: worker.Config.Labels[limitsLabel]}
	helper.ID = "cache-init-" + key
	helper.Name = "/pulse-cache-init-" + key
	helper.State.Status = "exited"
	d.containers[helper.ID] = helper
	if _, err := open(t.Context(), p.cfg, p.e, false); err != nil {
		t.Fatal("legacy helper prevents startup", err)
	}
	if _, ok := d.containers[helper.ID]; !ok {
		t.Fatal("legacy cache helper removed")
	}
}
