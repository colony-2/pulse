package main

import (
	"context"
	"flag"
	"github.com/colony-2/pulse/internal/supervisor"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	timeout := flag.Duration("timeout", 0, "execution timeout")
	before := flag.String("start-before", "", "latest start time")
	flag.Parse()
	var deadline time.Time
	if *before != "" {
		var err error
		deadline, err = time.Parse(time.RFC3339Nano, *before)
		if err != nil {
			os.Exit(125)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(supervisor.Run(ctx, flag.Args(), *timeout, deadline))
}
