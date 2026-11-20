package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/colony-2/pulse/pkg/pulsecli"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	pulsecli.RunnerMain()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return newRootCmd().ExecuteContext(ctx)
}
func newRootCmd() *cobra.Command { return pulsecli.NewCommand(version) }
