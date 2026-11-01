//go:build linux || darwin

package providerplugin

import (
	"os"
	"os/exec"
	"syscall"
)

// Keep helper children in an owned group. Native workloads must be managed by
// the backend independently; they must not be daemonized plugin descendants.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
func terminateHelpers(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func exitWithHelpers() {
	if group, e := syscall.Getpgid(0); e == nil && group == os.Getpid() {
		_ = syscall.Kill(-group, syscall.SIGKILL)
	}
	os.Exit(0)
}
