package supervisor

import (
	"context"
	"github.com/colony-2/pulse/pkg/compute"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExitAndTimeout(t *testing.T) {
	if n := Run(context.Background(), []string{"/bin/sh", "-c", "exit 7"}, time.Second, time.Time{}); n != 7 {
		t.Fatal(n)
	}
	start := time.Now()
	if n := Run(context.Background(), []string{"/bin/sh", "-c", "sleep 30"}, 30*time.Millisecond, time.Time{}); n != 124 {
		t.Fatal(n)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not terminate")
	}
	if n := Run(context.Background(), []string{"/bin/true"}, time.Second, time.Now().Add(-time.Second)); n != 125 {
		t.Fatal(n)
	}
}

func TestPrivateStdinIsPipedAndNotInherited(t *testing.T) {
	t.Setenv(compute.StdinEnv, "lease-capability\n")
	output := filepath.Join(t.TempDir(), "input")
	if n := Run(context.Background(), []string{"/bin/sh", "-c", `test -z "${PULSE_EXEC_STDIN+x}" || exit 9; cat > "$1"`, "child", output}, time.Second, time.Time{}); n != 0 {
		t.Fatal(n)
	}
	b, err := os.ReadFile(output)
	if err != nil || string(b) != "lease-capability\n" {
		t.Fatal("stdin handoff failed", err)
	}
}
