// Package providers constructs built-in and remote provider instances.
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providers/adapter"
	"github.com/colony-2/pulse/internal/providers/cloud"
	"github.com/colony-2/pulse/internal/providers/docker"
	pluginprovider "github.com/colony-2/pulse/internal/providers/plugin"
	"github.com/colony-2/pulse/internal/providers/remote"
	"github.com/colony-2/pulse/internal/quantity"
	"github.com/colony-2/pulse/pkg/compute"
	"github.com/colony-2/pulse/pkg/providerplugin"
	"github.com/colony-2/pulse/pkg/providerregistry"
	"github.com/distribution/reference"
	"maps"
	"os"
	"time"
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
	for name, p := range cfg.Providers {
		switch p.Type {
		case "tart", "orchard":
			instance, cleanup, err := buildMac(ctx, name, p, cfg)
			if err != nil {
				return fail(err)
			}
			out[name] = instance
			closers = append(closers, cleanup)
		case "plugin":
			c, e := pluginprovider.New(ctx, name, p.Plugin, cfg.BaseDir, cfg.Call)
			if e != nil {
				return fail(e)
			}
			out[name] = wrap(c, p.Type, cfg)
			closers = append(closers, c.Close)
		case "remote":
			if p.TokenEnv == "" {
				return fail(fmt.Errorf("remote provider %s requires token_env", name))
			}
			envKey := p.TokenEnv
			if os.Getenv(envKey) == "" {
				return fail(fmt.Errorf("missing provider token environment %s", envKey))
			}
			c, e := remote.New(p.Endpoint, nil, func() (string, error) {
				v := os.Getenv(envKey)
				if v == "" {
					return "", fmt.Errorf("missing provider token environment %s", envKey)
				}
				return v, nil
			}, p.AllowHTTP)
			if e != nil {
				return fail(e)
			}
			out[name] = wrap(c, p.Type, cfg)
		case "docker":
			socket, e := docker.ResolveSocket(p.Socket)
			if e != nil {
				return fail(e)
			}
			var cpu, mem int64
			if p.Capacity.CPU != "" {
				cpu, e = quantity.Parse(p.Capacity.CPU, true)
				if e != nil {
					return fail(e)
				}
			}
			if p.Capacity.Memory != "" {
				mem, e = quantity.Parse(p.Capacity.Memory, false)
				if e != nil {
					return fail(e)
				}
			}
			overhead := int64(256 << 20)
			if p.Overhead != "" {
				overhead, e = quantity.Parse(p.Overhead, false)
				if e != nil {
					return fail(e)
				}
			}
			dc := docker.Config{Socket: socket, LockDir: p.LockDir, ScratchPath: p.ScratchPath, RegistryAuth: os.Getenv(p.RegistryAuthEnv), CPUMillis: cpu, MemoryBytes: mem, Overhead: overhead, MaxContainers: p.Capacity.MaxContainers}
			if p.DependencyCache != nil {
				cache := p.DependencyCache
				dc.DependencyCache = docker.CacheConfig{Disabled: cache.Enabled != nil && !*cache.Enabled, Namespace: cache.Namespace, Generation: cache.Generation}
				dc.DependencyCache.MaxAge, e = time.ParseDuration(cache.MaxAge)
				if e != nil {
					return fail(e)
				}
			}
			if prior, ok := locals[socket]; ok {
				if prior.config != dc {
					return fail(fmt.Errorf("Docker aliases must share an identical budget/configuration"))
				}
				out[name] = out[prior.name]
				continue
			}
			c, e := docker.New(ctx, dc)
			if e != nil {
				return fail(e)
			}
			out[name] = wrap(c, p.Type, cfg)
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
			out[name] = wrap(c, p.Type, cfg)
		}
	}
	return out, closeAll, nil
}

func wrap(native compute.Provider, kind string, cfg *config.Config) provider.Provider {
	targets := map[string]string{}
	for _, t := range cfg.Targets {
		targets[t.Instance+"\x00"+t.Tenant] = t.JobDB
	}
	p := &adapter.Provider{Native: native, Env: cfg.Defaults.Env, Targets: targets}
	switch kind {
	case "cloudrun", "azurejobs":
		p.Platforms = []string{"linux/amd64"}
	case "ecs":
		p.Platforms = []string{"linux/arm64", "linux/amd64"}
	case "docker":
		d := native.(*docker.Provider)
		p.Platforms = d.SupportedPlatforms()
	}
	if h, ok := native.(interface{ Health() providerplugin.Health }); ok {
		return &pluginAdapter{Provider: p, health: h}
	}
	return p
}

type pluginAdapter struct {
	*adapter.Provider
	health interface{ Health() providerplugin.Health }
}

func (p *pluginAdapter) Health() providerplugin.Health { return p.health.Health() }
