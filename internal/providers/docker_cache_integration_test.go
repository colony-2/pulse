package providers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerprovider "github.com/colony-2/pulse/internal/providers/docker"
	"github.com/colony-2/pulse/pkg/compute"
)

// TestDockerDependencyCacheIntegration builds a test-only worker from Pulse's
// pinned c2j module. The published base may lag the latest c2j release; the
// separate supplied-lease integration still tests its unmodified c2j binary.
func TestDockerDependencyCacheIntegration(t *testing.T) {
	socket, err := dockerprovider.ResolveSocket("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Match provider resolution: reuse a local image, pulling only if absent.
	if err := exec.CommandContext(ctx, "docker", "--host", socket, "image", "inspect", c2jIntegrationImage).Run(); err != nil {
		docker("pull", c2jIntegrationImage)
	}
	platform := docker("image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", c2jIntegrationImage)
	_, arch, _ := strings.Cut(platform, "/")
	dir := t.TempDir()
	worker := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "cache-worker"), "./testdata/cache-worker")
	worker.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := worker.CombinedOutput(); err != nil {
		t.Fatalf("build c2j tool worker: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM "+c2jIntegrationImage+"\nCOPY cache-worker /usr/local/bin/cache-worker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	image := "ghcr.io/colony-2/base:pulse-cache-test-" + compute.NewID()
	docker("build", "--network=none", "-t", image, dir)
	t.Cleanup(func() { exec.Command("docker", "--host", socket, "image", "rm", image).CombinedOutput() })
	namespace := "integration-" + compute.NewID()
	config := dockerprovider.Config{Socket: socket, LockDir: t.TempDir(), CPUMillis: 2000, MemoryBytes: 6 << 30, MaxContainers: 2, DependencyCache: dockerprovider.CacheConfig{Namespace: namespace}}
	p, err := dockerprovider.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	keys := map[string]bool{}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		// Failed submissions may create an initializer without a worker. This
		// derived image is unique to the test, so discover those records too.
		helpers, err := exec.CommandContext(cleanup, "docker", "--host", socket, "ps", "-aq", "--filter", "ancestor="+image, "--filter", "label=pulse_resource_role=cache-init").CombinedOutput()
		if err != nil {
			t.Errorf("list cache initializers: %v: %s", err, helpers)
		}
		for _, id := range strings.Fields(string(helpers)) {
			key, err := exec.CommandContext(cleanup, "docker", "--host", socket, "inspect", "--format", `{{index .Config.Labels "pulse_cache_key"}}`, id).CombinedOutput()
			if err != nil {
				t.Errorf("inspect cache initializer: %v: %s", err, key)
				continue
			}
			keys[strings.TrimSpace(string(key))] = true
		}
		for key := range keys {
			if out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", "pulse-cache-init-"+key).CombinedOutput(); err != nil && !strings.Contains(string(out), "No such container") {
				t.Errorf("remove cache initializer: %v: %s", err, out)
			}
			for _, kind := range []string{"nix", "tools"} {
				if out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "volume", "rm", "pulse-cache-v1-"+key+"-"+kind).CombinedOutput(); err != nil && !strings.Contains(string(out), "no such volume") {
					t.Errorf("remove cache volume: %v: %s", err, out)
				}
			}
		}
	})
	launch := func(tenant, mode string) (compute.Launch, string) {
		id := compute.NewID()
		l := compute.Launch{Request: compute.Request{LaunchID: id, Allocation: compute.Allocation{Image: image, Platform: platform, CPUMillis: 1000, MemoryBytes: 2 << 30, ScratchBytes: 256 << 20}, Metadata: map[string]string{"pulse_job_id": id, "pulse_jobdb_instance_id": namespace, "pulse_tenant_id": tenant}}, Process: compute.Process{Command: []string{"/bin/sh"}, Args: []string{"-ec", "while [ ! -e /tmp/pulse-test-release ]; do sleep 0.1; done; exec cache-worker " + mode}, Env: map[string]string{}}}
		if mode == "warm" {
			// Reused environments must not need any package registry or Nix source.
			l.Process.Env["UV_OFFLINE"] = "1"
			l.Process.Env["pnpm_config_offline"] = "true"
		}
		// Register before Submit: a timed-out worker create may still appear.
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
			defer stop()
			ids, err := exec.CommandContext(cleanup, "docker", "--host", socket, "ps", "-aq", "--filter", "label=pulse_launch_id="+id).CombinedOutput()
			if err != nil {
				t.Errorf("list cache workers: %v: %s", err, ids)
				return
			}
			for _, cid := range strings.Fields(string(ids)) {
				if out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", cid).CombinedOutput(); err != nil && !strings.Contains(string(out), "No such container") {
					t.Errorf("remove cache worker: %v: %s", err, out)
				}
			}
		})
		out, err := p.Submit(ctx, []compute.Launch{l})
		if err != nil || len(out) != 1 || out[0].Status != compute.Accepted {
			t.Fatalf("submit: %+v %v", out, err)
		}
		ids := strings.Fields(docker("ps", "-aq", "--filter", "label=pulse_launch_id="+id))
		if len(ids) != 1 {
			t.Fatalf("missing worker: %v", ids)
		}
		cid := ids[0]
		var state struct {
			Config struct{ Labels map[string]string }
			Mounts []struct {
				Name, Destination string
				RW                bool
			}
		}
		if err := json.Unmarshal([]byte(docker("inspect", "--format", "{{json .}}", cid)), &state); err != nil {
			t.Fatal(err)
		}
		key := state.Config.Labels["pulse_cache_key"]
		if key == "" {
			t.Fatal("cache not attached")
		}
		keys[key] = true
		if len(state.Mounts) != 2 {
			t.Fatalf("unexpected mounts: %+v", state.Mounts)
		}
		for _, m := range state.Mounts {
			if !m.RW || !strings.Contains(m.Name, key) {
				t.Fatalf("invalid cache mount: %+v", m)
			}
		}
		if mode == "warm" {
			docker("network", "disconnect", "bridge", cid)
		}
		return l, cid
	}
	concurrentFinish := func(a, b string) string {
		first, second := observeDocker(t, ctx, socket, a), observeDocker(t, ctx, socket, b)
		docker("exec", a, "/bin/sh", "-c", "touch /tmp/pulse-test-release")
		docker("exec", b, "/bin/sh", "-c", "touch /tmp/pulse-test-release")
		logs := ""
		for _, o := range []*dockerObservation{first, second} {
			code, out := o.finish(t, ctx)
			if code != "0" {
				t.Fatalf("concurrent cache worker exit %s:\n%s", code, out)
			}
			t.Log(out)
			logs += out
		}
		return logs
	}
	_, cold := launch("tools", "cold")
	_, coldPeer := launch("tools", "cold")
	coldLogs := concurrentFinish(cold, coldPeer)
	if !strings.Contains(coldLogs, `"outcome":"prepared"`) {
		t.Fatal("cold setup not recorded", coldLogs)
	}
	// A fresh provider reconstructs readiness from Docker, without local state.
	p.Close()
	p, err = dockerprovider.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	_, warmA := launch("tools", "warm")
	_, warmB := launch("tools", "warm")
	if len(keys) != 1 {
		t.Fatal("compatible jobs did not share one namespace")
	}
	concurrentFinish(warmA, warmB)
	// Reset by changing generation; old volumes survive while the new pair is seeded.
	p.Close()
	config.DependencyCache.Generation = "2"
	p, err = dockerprovider.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	l, reset := launch("tools", "cold")
	if len(keys) != 2 {
		t.Fatal("generation change reused old store")
	}
	// No need to reinstall packages to establish isolation: inspect the empty cache.
	if got := docker("exec", reset, "/bin/sh", "-ec", "find /var/cache/pulse/tools -name ready.json | wc -l"); got != "0" {
		t.Fatal("new generation contains old tools", got)
	}
	docker("rm", "-f", reset)
	t.Logf("generation reset created fresh stores for %s", l.LaunchID)
	// Automatic retirement removes only idle pairs and recreates them on demand.
	p.Close()
	config.DependencyCache.MaxAge = time.Nanosecond
	p, err = dockerprovider.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	for key := range keys {
		out, err := exec.CommandContext(ctx, "docker", "--host", socket, "volume", "inspect", "pulse-cache-v1-"+key+"-tools").CombinedOutput()
		if err == nil {
			t.Fatalf("idle expired cache survived: %s", out)
		}
	}
	fmt.Fprintln(os.Stderr, "Docker dependency cache cold/warm/concurrent/restart/retirement checks passed")
}
