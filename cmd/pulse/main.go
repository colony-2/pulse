package main

import (
	"context"
	"fmt"
	"github.com/colony-2/pulse/pkg/pulsecli"
	"github.com/spf13/cobra"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
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
