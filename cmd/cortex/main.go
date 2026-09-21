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
	check := flag.Bool("check", false, "validate configuration and c2j executable without submitting")
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
	cli := &c2j.Client{Executable: cfg.C2J.Executable, WorkingDir: cfg.C2J.WorkingDir, ExpectedVersion: cfg.C2J.ExpectedVersion, Env: cfg.C2J.Env}
	checkCtx, stop := context.WithTimeout(ctx, cfg.Call)
	e = cli.Check(checkCtx)
	stop()
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
	if *check {
		fmt.Println("configuration, c2j executable, and providers are valid")
		return nil
	}
	c := &controller.Controller{Config: cfg, Lister: cli, Scheduler: scheduler.New(cfg.Cool, cfg.Call), Providers: instances, Log: slog.New(slog.NewJSONHandler(os.Stderr, nil))}
	if *once {
		return c.Once(ctx)
	}
	e = c.Run(ctx)
	if errors.Is(e, context.Canceled) {
		return nil
	}
	return e
}
