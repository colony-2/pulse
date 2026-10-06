package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/controller"
	"github.com/colony-2/pulse/internal/httpapi"
	"github.com/colony-2/pulse/internal/providers"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/spf13/cobra"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return newRootCmd().ExecuteContext(ctx)
}

func newRootCmd() *cobra.Command {
	var path, jobdb string
	var once bool
	root := &cobra.Command{Use: "pulse", Short: "Run c2j jobs for a JobDB tenant", SilenceUsage: true, SilenceErrors: true}
	execute := func(check bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("config") && path == "" {
				return fmt.Errorf("--config requires a nonempty file path")
			}
			if cmd.Flags().Changed("jobdb") && strings.TrimSpace(jobdb) == "" {
				return fmt.Errorf("--jobdb requires a nonempty tenant URI")
			}
			cfg, err := config.LoadOptions(cmd.Context(), path, jobdb)
			if err != nil {
				return err
			}
			return runPulse(cmd.Context(), cfg, once, check, cmd.OutOrStdout())
		}
	}
	root.PersistentFlags().StringVar(&path, "config", "", "optional configuration file (overrides PULSE_CONFIG and pulse.yaml)")
	root.PersistentFlags().StringVar(&jobdb, "jobdb", "", "JobDB tenant URI (defaults to C2J_JOBDB or .c2j/config.yaml)")
	root.Args, root.RunE = cobra.NoArgs, execute(false)
	runCmd := &cobra.Command{Use: "run", Short: "Run jobs for the selected tenant", Args: cobra.NoArgs, RunE: execute(false)}
	runCmd.Flags().BoolVar(&once, "once", false, "run one discovery/submission pass")
	root.AddCommand(runCmd,
		&cobra.Command{Use: "check", Short: "Validate settings and initialize providers without submitting jobs", Args: cobra.NoArgs, RunE: execute(true)},
		&cobra.Command{Use: "version", Short: "Print pulse build information", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "pulse version %s\n", version)
			return err
		}},
	)
	return root
}

func runPulse(parent context.Context, cfg *config.Config, once, check bool, out io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	connections := make([]c2j.Connection, 0, len(cfg.Targets))
	for _, target := range cfg.Targets {
		connections = append(connections, c2j.Connection{URI: target.JobDB, TokenEnv: target.JobDBTokenEnv})
	}
	lister, e := c2j.NewEmbedded(connections)
	if e != nil {
		return e
	}
	providerCtx, providerCancel := context.WithTimeout(ctx, cfg.Call)
	instances, closeProviders, e := providers.Build(providerCtx, cfg)
	providerCancel()
	if e != nil {
		return e
	}
	defer closeProviders()
	if check {
		fmt.Fprintln(out, "configuration, listing backend, and providers are valid")
		return nil
	}
	sched := scheduler.New(cfg.Cool, cfg.Call)
	sched.ClaimConcurrency, sched.ClaimTimeout = cfg.ClaimConcurrency, cfg.Claim
	c := &controller.Controller{Config: cfg, Lister: lister, Claimer: lister, Scheduler: sched, Providers: instances, Log: slog.New(slog.NewJSONHandler(os.Stderr, nil))}
	if once {
		return c.Once(ctx)
	}
	api, e := httpapi.New(c, version)
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp", cfg.HTTP.Listen)
	if e != nil {
		return fmt.Errorf("listen for HTTP diagnostics: %w", e)
	}
	server := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, WriteTimeout: cfg.Call + 10*time.Second, MaxHeaderBytes: 64 << 10}
	httpDone := make(chan error, 1)
	go func() { httpDone <- server.Serve(listener) }()
	c.Log.Info("read-only HTTP API listening", "address", listener.Addr().String())
	controllerDone := make(chan error, 1)
	go func() { controllerDone <- c.Run(ctx) }()
	select {
	case e = <-httpDone:
		cancel()
		<-controllerDone
	case e = <-controllerDone:
		cancel()
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
