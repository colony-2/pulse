package providers_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	docker "github.com/colony-2/pulse-docker"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providers"
)

// Alternative execution is conditional on an independent successful probe. An
// infrastructure error cannot stand in for evidence of an unsupported machine.
func TestDockerMultiPlatformIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	socket, err := docker.ResolveSocket("")
	if err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	native, err := command("info", "--format", "{{.OSType}}/{{.Architecture}}")
	if err != nil {
		t.Fatal(native, err)
	}
	native = strings.ReplaceAll(strings.ReplaceAll(native, "aarch64", "arm64"), "x86_64", "amd64")
	alternative := "linux/amd64"
	if native == alternative {
		alternative = "linux/arm64"
	} else if native != "linux/arm64" {
		t.Fatal("unsupported daemon", native)
	}
	out, probeErr := command("run", "--rm", "--network", "none", "--platform", alternative, "alpine:3.21", "/bin/true")
	if probeErr != nil && !strings.Contains(strings.ToLower(out), "exec format error") {
		t.Fatalf("alternative probe inconclusive: %v: %s", probeErr, out)
	}
	cfg, err := config.Parse([]byte(`providers: {docker: {type: docker}}
targets: [{jobdb: 'http://localhost/platform-test', launch_services: [{name: docker, priority: 1}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	instances, closeAll, err := providers.Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	p := instances["docker"]
	caps, err := p.GetCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := "ubuntu-26.04-" + strings.TrimPrefix(alternative, "linux/")
	advertised := false
	for _, b := range caps.Bases {
		if b.Name == base {
			advertised = true
		}
	}
	t.Logf("native=%s alternative=%s independent_exit_success=%v advertised=%v", native, alternative, probeErr == nil, advertised)
	if probeErr == nil {
		if !advertised {
			t.Fatal("independently executable platform not advertised")
		}
		closeAll()
		runDockerC2J(t, alternative, []string{"peer connectivity"})
		return
	}
	if advertised {
		t.Fatal("failed execution advertised")
	}
	id := fmt.Sprintf("pulse-unsupported-%d", time.Now().UnixNano())
	results, _, err := p.Submit(ctx, []api.Launch{{LaunchID: id, Allocation: api.Allocation{Base: base}, Job: api.JobRef{JobDB: "http://localhost/platform-test", InstanceID: "test", TenantID: "platform-test", JobID: "job"}, Lease: "synthetic-unused-lease"}}, api.SubmitOptions{})
	if err != nil || len(results) != 1 || results[0].Status != api.Unsupported {
		t.Fatal("unsupported platform did not decline", results, err)
	}
	workers, err := command("ps", "-aq", "--filter", "label=pulse_launch_id="+id)
	if err != nil || workers != "" {
		t.Fatal("unsupported platform created worker", workers, err)
	}
	t.Logf("alternative execution unavailable (independent exec-format failure); public unsupported/no-worker behavior verified: %s", out)
}
