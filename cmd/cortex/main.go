package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/colony-2/cortex/internal/c2j"
	"github.com/colony-2/cortex/internal/config"
	"github.com/colony-2/cortex/internal/controller"
	"github.com/colony-2/cortex/internal/httpapi"
	"github.com/colony-2/cortex/internal/providers"
	"github.com/colony-2/cortex/internal/scheduler"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	path := flag.String("config", "", "configuration file (overrides CORTEX_CONFIG; default: CORTEX_CONFIG or ./cortex.yaml)")
	once := flag.Bool("once", false, "run one discovery/submission pass")
	check := flag.Bool("check", false, "validate configuration, listing backend and providers without submitting")
	ver := flag.Bool("version", false, "print version")
	flag.Parse()
	if *ver {
		fmt.Println("cortex " + version)
		return nil
	}
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	var explicitConfig bool
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})
	if explicitConfig && *path == "" {
		return fmt.Errorf("-config requires a nonempty file path")
	}
	cfg, e := config.LoadSource(*path)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var lister controller.Lister
	if cfg.C2J.Mode == "external" {
		cli := &c2j.Client{Executable: cfg.C2J.Executable, WorkingDir: cfg.C2J.WorkingDir, ExpectedVersion: cfg.C2J.ExpectedVersion, Env: cfg.C2J.Env}
		checkCtx, stop := context.WithTimeout(ctx, cfg.Call)
		e = cli.Check(checkCtx)
		stop()
		if e != nil {
			return e
		}
		lister = cli
	} else {
		connections := make([]c2j.Connection, 0, len(cfg.Targets))
		for _, target := range cfg.Targets {
			connections = append(connections, c2j.Connection{URI: target.JobDB, TokenEnv: target.JobDBTokenEnv})
		}
		lister, e = c2j.NewEmbedded(connections)
		if e != nil {
			return e
		}
	}
	providerCtx, providerCancel := context.WithTimeout(ctx, cfg.Call)
	instances, closeProviders, e := providers.Build(providerCtx, cfg)
	providerCancel()
	if e != nil {
		return e
	}
	defer closeProviders()
	if *check {
		fmt.Println("configuration, listing backend, and providers are valid")
		return nil
	}
	c := &controller.Controller{Config: cfg, Lister: lister, Scheduler: scheduler.New(cfg.Cool, cfg.Call), Providers: instances, Log: slog.New(slog.NewJSONHandler(os.Stderr, nil))}
	if *once {
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
