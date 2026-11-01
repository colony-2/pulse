//go:build !linux && !darwin

package providerplugin

import (
	"os"
	"os/exec"
)

// Pulse's supported controller targets are Linux and macOS.
func isolateProcess(cmd *exec.Cmd) {}
func terminateHelpers(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func exitWithHelpers() { os.Exit(0) }
