package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Run supervises a process group so timeout enforcement survives Pulse exit.
func Run(ctx context.Context, argv []string, timeout time.Duration, startBefore time.Time) int {
	if len(argv) == 0 || timeout <= 0 {
		return 125
	}
	if !startBefore.IsZero() && !time.Now().Before(startBefore) {
		return 125
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 125
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			return 0
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return 125
	case <-ctx.Done():
	case <-timer.C:
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	grace := time.NewTimer(2 * time.Second)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return 124
}
