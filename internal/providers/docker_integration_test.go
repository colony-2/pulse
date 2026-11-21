package providers_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Default provider selection must initialize Docker without host helper tools.
func TestDockerIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "pulse")
	if out, err := exec.CommandContext(ctx, "../../scripts/build-pulse.sh", binary).CombinedOutput(); err != nil {
		t.Fatalf("prepare Pulse binary: %v\n%s", err, out)
	}
	cmd := exec.CommandContext(ctx, binary, "check")
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name != "PULSE_CONFIG" && name != "C2J_JOBDB" && name != "PATH" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "C2J_JOBDB=http://localhost/acme", "PATH="+t.TempDir())
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		cmd.Env = append(cmd.Env, "PULSE_CONFIG=providers: {docker: {type: docker}}\ntargets: [{jobdb: 'http://localhost/acme', launch_services: [{name: docker, priority: 1}]}]")
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "providers are valid") {
		t.Fatalf("default Docker CLI initialization: %v\n%s", err, out)
	}
	t.Logf("default Docker CLI: %s", out)
}
