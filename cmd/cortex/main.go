package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/colony-2/cortex/internal/c2j"
	"github.com/colony-2/cortex/internal/config"
	"github.com/colony-2/cortex/internal/controller"
	"github.com/colony-2/cortex/internal/providers"
	"github.com/colony-2/cortex/internal/scheduler"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "cortex.yaml", "configuration file")
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
	cfg, e := config.Load(*path)
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
	e = c.Run(ctx)
	if errors.Is(e, context.Canceled) {
		return nil
	}
	return e
}
