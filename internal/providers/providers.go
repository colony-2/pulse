// Package providers constructs built-in and remote provider instances.
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"

	docker "github.com/colony-2/pulse-docker"
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providers/adapter"
	"github.com/colony-2/pulse/internal/providers/cloud"
	pluginprovider "github.com/colony-2/pulse/internal/providers/plugin"
	"github.com/colony-2/pulse/internal/providers/remote"
	"github.com/colony-2/pulse/internal/quantity"
	"github.com/colony-2/pulse/pkg/providerplugin"
	"github.com/colony-2/pulse/pkg/providerregistry"
	"github.com/distribution/reference"
)

func Build(ctx context.Context, cfg *config.Config) (map[string]provider.Provider, func(), error) {
	out := map[string]provider.Provider{}
	closers := []func(){}
	closeAll := func() {
		for _, f := range closers {
			f()
		}
	}
	fail := func(e error) (map[string]provider.Provider, func(), error) { closeAll(); return nil, func() {}, e }
	type local struct {
		config docker.Config
		name   string
	}
	locals := map[string]local{}
	// Native resources created before endpoint labels used the configured target.
	targets := make(map[string]string, len(cfg.Targets))
	for _, t := range cfg.Targets {
		targets[t.Instance+"\x00"+t.Tenant] = t.JobDB
	}
	for name, p := range cfg.Providers {
		switch p.Type {
		case "vz":
			instance, cleanup, err := buildMac(ctx, name, p, cfg)
			if err != nil {
				return fail(err)
			}
			out[name] = instance
			closers = append(closers, cleanup)
		case "plugin":
			if p.Plugin != nil && p.Plugin.Endpoint != "" {
				if err := p.Plugin.ValidateEndpoint(); err != nil {
					return fail(err)
				}
				if len(cfg.Defaults.Env) != 0 {
					return fail(fmt.Errorf("HTTP plugin provider %s requires executor environment to be configured at the endpoint", name))
				}
				token, err := envToken(p.Plugin.TokenEnv)
				if err != nil {
					return fail(err)
				}
				c, err := remote.NewStructured(p.Plugin.Endpoint, &http.Client{Timeout: cfg.Call}, token, p.Plugin.AllowHTTP)
				if err != nil {
					return fail(err)
				}
				out[name] = c
				continue
			}
			options, e := p.Plugin.ProcessConfig(name, cfg.BaseDir)
			if e != nil {
				return fail(e)
			}
			options.CallTimeout = cfg.Call
			if p.Plugin.ProtocolVersion() == providerplugin.ProtocolVersion {
				c, err := providerplugin.Start(ctx, options)
				if err != nil {
					return fail(fmt.Errorf("plugin provider %s: %w", name, err))
				}
				out[name] = &pluginAdapter{Provider: &adapter.Provider{Native: c, Env: cfg.Defaults.Env, Targets: targets}, process: c}
				closers = append(closers, c.Close)
				continue
			}
			c, err := pluginprovider.Start(ctx, options, cfg.Defaults.Env)
			if err != nil {
				return fail(fmt.Errorf("plugin provider %s: %w", name, err))
			}
			out[name] = c
			closers = append(closers, func() {
				if err := c.Close(); err != nil {
					slog.Warn("plugin provider cleanup failed", "provider", name, "error", err)
				}
			})
		case "remote":
			if p.TokenEnv == "" {
				return fail(fmt.Errorf("remote provider %s requires token_env", name))
			}
			token, err := envToken(p.TokenEnv)
			if err != nil {
				return fail(err)
			}
			c, e := remote.New(p.Endpoint, nil, token, p.AllowHTTP)
			if e != nil {
				return fail(e)
			}
			out[name] = &adapter.Provider{Native: c, Env: cfg.Defaults.Env, Targets: targets}
		case "docker":
			dc, e := dockerSettings(p, cfg)
			if e != nil {
				return fail(e)
			}
			socket := dc.Socket
			if prior, ok := locals[socket]; ok {
				if !sameDockerSettings(prior.config, dc) {
					return fail(fmt.Errorf("Docker aliases must share an identical budget/configuration"))
				}
				out[name] = out[prior.name]
				continue
			}
			c, e := docker.New(ctx, dc)
			if e != nil {
				return fail(e)
			}
			out[name] = c
			locals[socket] = local{dc, name}
			closers = append(closers, c.Close)
		default:
			if factory, ok := providerregistry.Lookup(p.Type); ok {
				options := p.Options
				if options == nil {
					options = map[string]any{}
				}
				raw, err := json.Marshal(options)
				if err != nil {
					return fail(fmt.Errorf("invalid provider configuration for %s", name))
				}
				instance, cleanup, err := factory(ctx, providerregistry.Options{Name: name, BaseDir: cfg.BaseDir, Config: raw, Env: maps.Clone(cfg.Defaults.Env)})
				if cleanup != nil {
					closers = append(closers, cleanup)
				}
				if err != nil || instance == nil {
					return fail(fmt.Errorf("Go provider %s initialization failed", name))
				}
				out[name] = instance
				continue
			}
			bounds := map[string]int64{}
			for image, value := range p.ImageStorageBounds {
				ref, e := reference.ParseNormalizedNamed(image)
				if e != nil {
					return fail(e)
				}
				n, e := quantity.Parse(value, false)
				if e != nil {
					return fail(e)
				}
				bounds[reference.TagNameOnly(ref).String()] = n
			}
			c, e := cloud.New(cloud.Config{Kind: p.Type, Project: p.Project, Region: p.Region, ServiceAccount: p.ServiceAccount, Cluster: p.Cluster, ExecutionRole: p.ExecutionRole, TaskRole: p.TaskRole, Subnets: p.Subnets, SecurityGroups: p.SecurityGroups, PublicIP: p.PublicIP, Subscription: p.Subscription, ResourceGroup: p.ResourceGroup, EnvironmentID: p.EnvironmentID, SupervisorPath: p.SupervisorPath, ImageStorageBounds: bounds, MaxAzureCPU: p.MaxAzureCPU, TokenEnv: p.TokenEnv})
			if e != nil {
				return fail(e)
			}
			out[name] = c.InstanceProvider(cfg.Defaults.Env, targets)
		}
	}
	logProviders(ctx, slog.Default(), cfg, out)
	return out, closeAll, nil
}

func envToken(key string) (func() (string, error), error) {
	read := func() (string, error) {
		value := os.Getenv(key)
		if value == "" {
			return "", fmt.Errorf("missing provider token environment %s", key)
		}
		return value, nil
	}
	_, err := read()
	return read, err
}

type pluginAdapter struct {
	*adapter.Provider
	process *providerplugin.Process
}

func (p *pluginAdapter) Health() providerplugin.Health { return p.process.Health() }
