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
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providers"
	dockerprovider "github.com/colony-2/pulse/internal/providers/docker"
	"github.com/colony-2/pulse/pkg/compute"
)

// This test uses Pulse's default Docker discovery and requires real containers.
// Docker Desktop on macOS and local Linux daemons run the same test, without opt-in.
func TestDockerIntegration(t *testing.T) {
	socket, err := dockerprovider.ResolveSocket("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	docker := func(t *testing.T, args ...string) string {
		t.Helper()
		// Inspection and cleanup use the endpoint selected by Pulse itself.
		cmd := exec.CommandContext(ctx, "docker", append([]string{"--host", socket}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	platform := docker(t, "info", "--format", "{{.OSType}}/{{.Architecture}}")
	platform = strings.ReplaceAll(strings.ReplaceAll(platform, "aarch64", "arm64"), "x86_64", "amd64")
	goos, arch, ok := strings.Cut(platform, "/")
	if !ok || goos != "linux" || (arch != "amd64" && arch != "arm64") {
		t.Fatalf("unsupported Docker daemon platform: %s", platform)
	}
	t.Logf("running real container jobs through Pulse defaults: %s (%s)", socket, platform)
	dir := t.TempDir()
	// Omit providers and launch_services. No helper binary or host mount exists.
	data := `defaults:
  image: alpine:3.21
  cpu: "100m"
  memory: 256Mi
  scratch: 8Mi
targets:
  - instance_id: docker-integration
    jobdb: http://localhost/acme
`
	cfg, err := config.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	unenforced := false
	t.Run("CLI defaults without a helper", func(t *testing.T) {
		binary := filepath.Join(dir, "pulse")
		build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/pulse")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build Pulse: %v\n%s", err, out)
		}
		cmd := exec.CommandContext(ctx, binary, "check")
		cmd.Dir = dir
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "PULSE_CONFIG=") && !strings.HasPrefix(entry, "C2J_JOBDB=") && !strings.HasPrefix(entry, "PATH=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "C2J_JOBDB=http://localhost/acme", "PATH="+t.TempDir())
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
	if cfg.Allocation.Platform != platform {
		t.Fatalf("default platform %s does not match daemon %s", cfg.Allocation.Platform, platform)
	}
	provider := instances[cfg.Targets[0].Services[0].Name]
	if provider == nil {
		t.Fatal("default target did not resolve to a provider")
	}

	newLaunch := func(t *testing.T, script string) (compute.Launch, string) {
		t.Helper()
		id := fmt.Sprintf("docker-test-%d", time.Now().UnixNano())
		sum := sha256.Sum256([]byte(id))
		name := "pulse-" + hex.EncodeToString(sum[:16])
		// Remove only this test's containers, including when submission fails.
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			if t.Failed() {
				cmd := exec.CommandContext(cleanupCtx, "docker", "--host", socket, "inspect", "--format", "{{json .State}}", name)
				out, _ := cmd.CombinedOutput()
				t.Logf("container state: %s", out)
			}
			cmd := exec.CommandContext(cleanupCtx, "docker", "--host", socket, "rm", "-f", name)
			out, err := cmd.CombinedOutput()
			if err != nil && !strings.Contains(string(out), "No such container") {
				t.Errorf("cleanup %s: %v: %s", name, err, out)
			}
		})
		return compute.Launch{
			Request: compute.Request{LaunchID: id, Allocation: cfg.Allocation, Metadata: map[string]string{"pulse_job_id": id}},
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
		l, name := newLaunch(t, `test "$(wc -c)" -eq 163840
test "${PULSE_EXEC_STDIN+x}" = ''
test "$LITERAL" = '$HOME'
test "$PWD" = /scratch
test "$TMPDIR" = /scratch
test "$PULSE_SCRATCH_DIR" = /scratch
grep -q ' /scratch tmpfs ' /proc/mounts
printf 'scratch works' > "$TMPDIR/result"
cat "$TMPDIR/result"`)
		l.Process.Stdin = compute.SecretInput(strings.Repeat("finite stdin payload", 8192))
		l.Process.Env["LITERAL"] = "$HOME"
		l.Process.WorkingDir = "/scratch"
		submit(t, l, compute.Accepted)
		if out := wait(t, name, "0"); out != "scratch works" {
			t.Fatalf("unexpected job output: %q", out)
		}
		var inspected []struct {
			Config struct {
				Labels     map[string]string
				Entrypoint []string
				Env        []string
			}
			HostConfig struct {
				NanoCPUs, Memory, MemorySwap int64
				Binds                        []string
				Tmpfs                        map[string]string
				RestartPolicy                struct{ Name string }
			}
		}
		if err := json.Unmarshal([]byte(docker(t, "inspect", name)), &inspected); err != nil || len(inspected) != 1 {
			t.Fatalf("inspect: %v", err)
		}
		h := inspected[0].HostConfig
		if len(h.Binds) != 0 || len(inspected[0].Config.Entrypoint) != 1 || inspected[0].Config.Entrypoint[0] != "/bin/sh" {
			t.Fatal("job used a wrapper or host mount", inspected)
		}
		for _, env := range inspected[0].Config.Env {
			if strings.Contains(env, "finite stdin payload") {
				t.Fatal("stdin leaked into container metadata")
			}
		}
		cpu, memory, mode := int64(100000000), int64(264<<20), "enforced"
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
		l, name := newLaunch(t, "echo failed >&2; exit 23")
		submit(t, l, compute.Accepted)
		if out := wait(t, name, "23"); out != "failed" {
			t.Fatalf("stderr not preserved: %q", out)
		}
	})
	t.Run("expired start deadline", func(t *testing.T) {
		l, name := newLaunch(t, "echo should-not-run")
		before := time.Now().Add(-time.Minute)
		l.StartBefore = &before
		submit(t, l, compute.Rejected)
		if found := docker(t, "ps", "-aq", "--filter", "name=^/"+name+"$"); found != "" {
			t.Fatal("expired launch created a container")
		}
	})
	t.Run("capacity and execution survive provider restart", func(t *testing.T) {
		// Keep the job alive until all restart/accounting assertions finish.
		// A fixed sleep races slow daemon startup, especially in a macOS VM.
		l, name := newLaunch(t, `mkfifo /scratch/release
echo started
read -r signal < /scratch/release
test "$signal" = release
echo completed`)
		submit(t, l, compute.Accepted)
		docker(t, "exec", name, "/bin/sh", "-ec", "until [ -p /scratch/release ]; do sleep 0.1; done")
		closeProviders()
		instances, closeAgain, err := providers.Build(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(closeAgain)
		provider = instances["docker"]
		blocked, nextName := newLaunch(t, "echo next-job")
		submit(t, blocked, compute.NoCapacity)
		listed, err := provider.List(ctx, compute.ListRequest{LaunchID: l.LaunchID})
		if err != nil || len(listed.Items) != 1 || listed.Items[0].State != "running" {
			t.Fatalf("running job missing from listing: %+v, %v", listed, err)
		}
		docker(t, "exec", name, "/bin/sh", "-ec", "printf 'release\\n' > /scratch/release")
		if out := wait(t, name, "0"); out != "started\ncompleted" {
			t.Fatalf("controller restart interrupted execution: %q", out)
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
