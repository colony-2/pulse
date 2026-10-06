package providers_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providers"
	"github.com/colony-2/pulse/pkg/compute"
)

// This test requires a real, local Linux daemon and fails if containers cannot
// execute. It runs in the ordinary Go suite on every platform the provider supports.
func TestDockerIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the local Docker provider requires Linux")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	docker := func(t *testing.T, args ...string) string {
		t.Helper()
		// Match the provider's default socket, regardless of CLI context settings.
		cmd := exec.CommandContext(ctx, "docker", append([]string{"--host", "unix:///var/run/docker.sock"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	docker(t, "info")
	dir := t.TempDir()
	helper := filepath.Join(dir, "pulse-exec")
	build := exec.CommandContext(ctx, "go", "build", "-o", helper, "../../cmd/pulse-exec")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build supervisor: %v\n%s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Omit providers and launch_services: exercise the actual default-provider
	// construction, helper discovery, and image pull path used by Pulse.
	data := fmt.Sprintf(`defaults:
  image: alpine:3.21
  platform: linux/%s
  cpu: "100m"
  memory: 32Mi
  scratch: 8Mi
targets:
  - instance_id: docker-integration
    jobdb: http://localhost/acme
    cells: [github.com/acme/app]
`, runtime.GOARCH)
	cfg, err := config.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	unenforced := false
	t.Run("CLI defaults and sibling helper", func(t *testing.T) {
		binary := filepath.Join(dir, "pulse")
		build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/pulse")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build Pulse: %v\n%s", err, out)
		}
		cmd := exec.CommandContext(ctx, binary, "-check")
		cmd.Dir = dir
		cmd.Env = []string{"PULSE_CONFIG=" + data, "PATH=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "providers are valid") {
			t.Fatalf("default Docker CLI initialization: %v\n%s", err, out)
		}
		unenforced = strings.Contains(string(out), "WARN Docker resource limits unavailable")
	})
	instances, closeProviders, err := providers.Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeProviders)
	provider := instances[cfg.Targets[0].Services[0].Name]
	if provider == nil {
		t.Fatal("default target did not resolve to a provider")
	}

	newLaunch := func(t *testing.T, script string, timeout int64) (compute.Launch, string) {
		t.Helper()
		id := fmt.Sprintf("docker-test-%d", time.Now().UnixNano())
		sum := sha256.Sum256([]byte(id))
		name := "pulse-" + hex.EncodeToString(sum[:16])
		// Remove only this test's containers, including when submission fails.
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			if t.Failed() {
				cmd := exec.CommandContext(cleanupCtx, "docker", "--host", "unix:///var/run/docker.sock", "inspect", "--format", "{{json .State}}", name)
				out, _ := cmd.CombinedOutput()
				t.Logf("container state: %s", out)
			}
			cmd := exec.CommandContext(cleanupCtx, "docker", "--host", "unix:///var/run/docker.sock", "rm", "-f", name)
			out, err := cmd.CombinedOutput()
			if err != nil && !strings.Contains(string(out), "No such container") {
				t.Errorf("cleanup %s: %v: %s", name, err, out)
			}
		})
		return compute.Launch{
			Request: compute.Request{LaunchID: id, Allocation: cfg.Allocation, TimeoutSeconds: timeout, Metadata: map[string]string{"pulse_job_id": id}},
			Process: compute.Process{Command: []string{"/bin/sh"}, Args: []string{"-ec", script}, Env: map[string]string{}},
		}, name
	}
	submit := func(t *testing.T, l compute.Launch, want compute.Status) {
		t.Helper()
		results, err := provider.Submit(ctx, []compute.Launch{l})
		if err != nil || len(results) != 1 || results[0].Status != want {
			t.Fatalf("submit: got %+v, %v; want %s", results, err, want)
		}
	}
	wait := func(t *testing.T, name, code string) string {
		t.Helper()
		if got := docker(t, "wait", name); got != code {
			t.Fatalf("container exit: got %s, want %s; logs: %s", got, code, docker(t, "logs", name))
		}
		return docker(t, "logs", name)
	}

	t.Run("process contract and limits", func(t *testing.T) {
		l, name := newLaunch(t, `test "$(cat)" = 'finite stdin payload'
test "${PULSE_EXEC_STDIN+x}" = ''
test "$LITERAL" = '$HOME'
test "$PWD" = /scratch
test "$TMPDIR" = /scratch
test "$PULSE_SCRATCH_DIR" = /scratch
grep -q ' /scratch tmpfs ' /proc/mounts
printf 'scratch works' > "$TMPDIR/result"
cat "$TMPDIR/result"`, 30)
		l.Process.Stdin = "finite stdin payload"
		l.Process.Env["LITERAL"] = "$HOME"
		l.Process.WorkingDir = "/scratch"
		submit(t, l, compute.Accepted)
		if out := wait(t, name, "0"); out != "scratch works" {
			t.Fatalf("unexpected job output: %q", out)
		}
		var inspected []struct {
			Config     struct{ Labels map[string]string }
			HostConfig struct {
				NanoCPUs, Memory, MemorySwap int64
				Tmpfs                        map[string]string
				RestartPolicy                struct{ Name string }
			}
		}
		if err := json.Unmarshal([]byte(docker(t, "inspect", name)), &inspected); err != nil || len(inspected) != 1 {
			t.Fatalf("inspect: %v", err)
		}
		h := inspected[0].HostConfig
		cpu, memory, mode := int64(100000000), int64(40<<20), "enforced"
		if unenforced {
			cpu, memory, mode = 0, 0, "disabled"
		}
		if h.NanoCPUs != cpu || h.Memory != memory || h.MemorySwap != memory || inspected[0].Config.Labels["pulse_docker_limits"] != mode || h.Tmpfs["/scratch"] != "rw,size=8388608,mode=1777" || h.RestartPolicy.Name != "no" {
			t.Fatalf("incorrect resource enforcement: %+v", h)
		}
		// Retrying a completed launch must not execute the process a second time.
		started := docker(t, "inspect", "--format", "{{.State.StartedAt}}", name)
		submit(t, l, compute.Accepted)
		if got := docker(t, "inspect", "--format", "{{.State.StartedAt}}", name); got != started {
			t.Fatal("completed job restarted")
		}
	})
	t.Run("job failure", func(t *testing.T) {
		l, name := newLaunch(t, "echo failed >&2; exit 23", 30)
		submit(t, l, compute.Accepted)
		if out := wait(t, name, "23"); out != "failed" {
			t.Fatalf("stderr not preserved: %q", out)
		}
	})
	t.Run("expired start deadline", func(t *testing.T) {
		l, name := newLaunch(t, "echo should-not-run", 30)
		before := time.Now().Add(-time.Minute)
		l.StartBefore = &before
		submit(t, l, compute.Rejected)
		if found := docker(t, "ps", "-aq", "--filter", "name=^/"+name+"$"); found != "" {
			t.Fatal("expired launch created a container")
		}
	})
	t.Run("capacity and timeout survive provider restart", func(t *testing.T) {
		l, name := newLaunch(t, "echo started; sleep 60; echo should-not-run", 10)
		submit(t, l, compute.Accepted)
		closeProviders()
		instances, closeAgain, err := providers.Build(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(closeAgain)
		provider = instances["docker"]
		blocked, nextName := newLaunch(t, "echo next-job", 30)
		submit(t, blocked, compute.NoCapacity)
		listed, err := provider.List(ctx, compute.ListRequest{LaunchID: l.LaunchID})
		if err != nil || len(listed.Items) != 1 || listed.Items[0].State != "running" {
			t.Fatalf("running job missing from listing: %+v, %v", listed, err)
		}
		if out := wait(t, name, "124"); out != "started" {
			t.Fatalf("timeout failed to stop job: %q", out)
		}
		listed, err = provider.List(ctx, compute.ListRequest{LaunchID: l.LaunchID})
		if err != nil || len(listed.Items) != 0 {
			t.Fatalf("terminal job still active: %+v, %v", listed, err)
		}
		submit(t, blocked, compute.Accepted)
		if out := wait(t, nextName, "0"); out != "next-job" {
			t.Fatalf("capacity not reusable: %q", out)
		}
	})
}
