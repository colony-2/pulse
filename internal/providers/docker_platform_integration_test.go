package providers_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providers"
	dockerprovider "github.com/colony-2/pulse/internal/providers/docker"
	"github.com/colony-2/pulse/pkg/compute"
)

func TestDockerMultiPlatformIntegration(t *testing.T) {
	socket, err := dockerprovider.ResolveSocket("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	version, err := docker("version", "--format", "{{.Server.APIVersion}}")
	if err != nil {
		t.Fatal(err, version)
	}
	var major, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err != nil {
		t.Fatal(err)
	}
	if major == 1 && minor < 49 {
		t.Skip("platform discovery requires Docker API 1.49 or later")
	}
	cfg, err := config.Parse([]byte(`defaults: {image: "alpine:3.21"}
providers:
  docker:
    type: docker
    capacity: {max_containers: 2}
targets: [{jobdb: "http://localhost/platform-test", launch_services: [{name: docker, priority: 1}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["alias"] = cfg.Providers["docker"]
	instances, closeProviders, err := providers.Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProviders()
	p := nativeProvider(t, instances["docker"]).(*dockerprovider.Provider)
	if nativeProvider(t, instances["alias"]) != p {
		t.Fatal("aliases did not reuse discovery and admission")
	}
	if cfg.Allocation.Base != "oci:linux/arm64:docker.io/library/alpine:3.21" {
		t.Fatal("discovery changed default", cfg.Allocation.Base)
	}
	t.Logf("native=%s supported=%v", p.NativePlatform(), p.SupportedPlatforms())
	// Independently confirm executable candidate platforms with the Docker CLI;
	// the assertion catches discovery that accidentally advertises native only.
	for _, platform := range []string{"linux/arm64", "linux/amd64"} {
		out, err := docker("run", "--rm", "--network", "none", "--platform", platform, "alpine:3.21", "/bin/true")
		if err == nil && !slices.Contains(p.SupportedPlatforms(), platform) {
			t.Fatalf("Docker executes %s but Pulse did not discover it", platform)
		}
		if err != nil {
			t.Logf("independent %s execution unavailable: %v: %s", platform, err, out)
		}
	}
	platforms := p.SupportedPlatforms()
	if len(platforms) < 2 {
		t.Skip("daemon does not provide both execution architectures; native execution covered separately")
	}
	var jobs []compute.Launch
	for i, platform := range platforms {
		id := fmt.Sprintf("platform-test-%d-%d", time.Now().UnixNano(), i)
		sum := sha256.Sum256([]byte(id))
		name := "pulse-" + hex.EncodeToString(sum[:16])
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", name).CombinedOutput()
			if err != nil && !strings.Contains(string(out), "No such container") {
				t.Errorf("cleanup: %v: %s", err, out)
			}
		})
		allocation := nativeAllocation(t, cfg.Allocation)
		allocation.Platform = platform
		job := compute.Launch{Request: compute.Request{LaunchID: id, Allocation: allocation, Metadata: map[string]string{"pulse_job_id": id}}, Process: compute.Process{Command: []string{"/bin/sleep"}, Args: []string{"300"}}}
		jobs = append(jobs, job)
		results, err := p.Submit(ctx, []compute.Launch{job})
		if err != nil || len(results) != 1 || results[0].Status != compute.Accepted {
			t.Fatalf("%s: %+v %v", platform, results, err)
		}
		arch, err := docker("exec", name, "/bin/uname", "-m")
		want := map[string]string{"linux/amd64": "x86_64", "linux/arm64": "aarch64"}[platform]
		if err != nil || arch != want {
			t.Fatalf("%s executed %q: %v", platform, arch, err)
		}
		// Verify the real container's immutable image against the requested variant.
		image, err := docker("image", "inspect", "alpine:3.21", "--format", "{{.Id}}")
		if err != nil {
			t.Fatal(err, image)
		}
		actual, err := docker("inspect", name, "--format", "{{.Image}}")
		if err != nil || actual != image {
			t.Fatalf("%s image: got %s want %s: %v", platform, actual, image, err)
		}
	}
	closeProviders()
	instances, closeAgain, err := providers.Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAgain()
	third := jobs[0]
	third.LaunchID += "-extra"
	results, err := nativeProvider(t, instances["alias"]).Submit(ctx, []compute.Launch{third})
	if err != nil || len(results) != 1 || results[0].Status != compute.NoCapacity {
		t.Fatalf("shared budget after restart: %+v %v", results, err)
	}
}
