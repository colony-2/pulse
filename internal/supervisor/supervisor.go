package supervisor

import (
	"context"
	"errors"
	"github.com/colony-2/pulse/pkg/compute"
	"os"
	"os/exec"
	"strings"
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
	// Native container APIs cannot supply finite stdin directly. The private
	// helper transport is converted to a pipe and excluded from the child env.
	if input, ok := os.LookupEnv(compute.StdinEnv); ok {
		if len(input) > compute.MaxStdin {
			return 125
		}
		cmd.Stdin = strings.NewReader(input)
	}
	cmd.Env = []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, compute.StdinEnv+"=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
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
