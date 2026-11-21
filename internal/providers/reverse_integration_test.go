package providers_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	docker "github.com/colony-2/pulse-docker"
	api "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/controller"
	"github.com/colony-2/pulse/internal/providers"
	"github.com/colony-2/pulse/internal/scheduler"
)

// The test fixture exposes a published address to Pulse and a Docker bridge
// address to its worker containers. Real workers use the configured JobDB URL.
type reverseDockerFixture struct {
	api.Provider
	workerJobDB string
}

func (p reverseDockerFixture) Submit(ctx context.Context, launches []api.Launch, options api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	launches = append([]api.Launch(nil), launches...)
	for i := range launches {
		launches[i].Job.JobDB = p.workerJobDB
	}
	return p.Provider.Submit(ctx, launches, options)
}

func runReverseDockerOnce(t *testing.T, ctx context.Context, jobDB, workerJobDB, socket string) ([]byte, error) {
	t.Helper()
	const name = "reverse-docker"
	const key = "reverse-docker-integration-test-key"
	t.Setenv("PULSE_TEST_REVERSE_KEY", key)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
reverse:
  listen: 127.0.0.1:0
  transport: http
providers:
  reverse-docker:
    type: reverse
    token_env: PULSE_TEST_REVERSE_KEY
targets:
  - jobdb: %q
    launch_services: [{name: reverse-docker, priority: 1}]
`, jobDB)))
	if err != nil {
		return nil, err
	}
	server, err := providers.StartReverse(ctx, cfg, false)
	if err != nil {
		return nil, err
	}
	defer server.Close()
	options := sdk.RunOptions{
		Address: server.Address, AuthMode: sdk.ReverseAuthMutualKey, Transport: sdk.ReverseTransportHTTP,
		MutualKey: []byte("wrong-key"), Init: sdk.Initialization{Name: name, BaseDir: t.TempDir()},
		StartupTimeout: 15 * time.Second, Serve: sdk.ServeOptions{Implementation: "reverse-test-docker", Version: "test"},
	}
	err = sdk.Run(ctx, func(context.Context, sdk.Init) (api.Provider, sdk.Cleanup, error) {
		t.Error("collector initialized a provider with an incorrect shared key")
		return nil, nil, errors.New("unauthorized test provider reached factory")
	}, options)
	if err == nil {
		return nil, errors.New("collector accepted an incorrect shared key")
	}
	provider, err := docker.New(ctx, docker.Config{
		Socket: socket, Overhead: 256 << 20, DependencyCache: docker.CacheConfig{Disabled: true},
	})
	if err != nil {
		return nil, err
	}
	defer provider.Close()
	providerCtx, stopProvider := context.WithCancel(ctx)
	done := make(chan error, 1)
	defer func() {
		stopProvider()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("reverse Docker provider stopped: %v", err)
		}
	}()
	options.MutualKey = []byte(key)
	go func() {
		done <- sdk.Run(providerCtx, func(context.Context, sdk.Init) (api.Provider, sdk.Cleanup, error) {
			return reverseDockerFixture{Provider: provider, workerJobDB: workerJobDB}, nil, nil
		}, options)
	}()
	readyCtx, stopReady := context.WithTimeout(ctx, options.StartupTimeout)
	defer stopReady()
	snapshot, err := server.Snapshot()
	for err == nil && len(snapshot.Providers) == 0 {
		snapshot, err = server.WaitForChange(readyCtx, snapshot.Revision)
	}
	if err != nil {
		return nil, err
	}
	lister, err := c2j.NewEmbedded([]c2j.Connection{{URI: jobDB}})
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	c := controller.Controller{
		Config: cfg, Lister: lister, Claimer: lister, Scheduler: scheduler.New(cfg.Cool, cfg.Call),
		Providers: server.Providers, Log: slog.New(slog.NewJSONHandler(&output, nil)),
	}
	err = c.Once(ctx)
	return output.Bytes(), err
}
