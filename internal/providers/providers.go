// Package providers constructs built-in and remote provider instances.
package providers

import (
	"context"
	"fmt"
	"github.com/colony-2/cortex/internal/config"
	"github.com/colony-2/cortex/internal/providers/cloud"
	"github.com/colony-2/cortex/internal/providers/docker"
	"github.com/colony-2/cortex/internal/providers/remote"
	"github.com/colony-2/cortex/internal/quantity"
	"github.com/colony-2/cortex/pkg/compute"
	"github.com/distribution/reference"
	"os"
)

func Build(ctx context.Context, cfg *config.Config) (map[string]compute.Provider, func(), error) {
	out := map[string]compute.Provider{}
	closers := []func(){}
	closeAll := func() {
		for _, f := range closers {
			f()
		}
	}
	fail := func(e error) (map[string]compute.Provider, func(), error) { closeAll(); return nil, func() {}, e }
	type local struct {
		config   docker.Config
		provider *docker.Provider
	}
	locals := map[string]local{}
	for name, p := range cfg.Providers {
		switch p.Type {
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
			out[name] = c
		case "docker":
			cpu, e := quantity.Parse(p.Capacity.CPU, true)
			if e != nil {
				return fail(e)
			}
			mem, e := quantity.Parse(p.Capacity.Memory, false)
			if e != nil {
				return fail(e)
			}
			overhead := int64(256 << 20)
			if p.Overhead != "" {
				overhead, e = quantity.Parse(p.Overhead, false)
				if e != nil {
					return fail(e)
				}
			}
			dc := docker.Config{Socket: p.Socket, Helper: p.Helper, LockDir: p.LockDir, ScratchPath: p.ScratchPath, RegistryAuth: os.Getenv(p.RegistryAuthEnv), CPUMillis: cpu, MemoryBytes: mem, Overhead: overhead, MaxContainers: p.Capacity.MaxContainers}
			if prior, ok := locals[p.Socket]; ok {
				if prior.config != dc {
					return fail(fmt.Errorf("Docker aliases must share an identical budget/configuration"))
				}
				out[name] = prior.provider
				continue
			}
			c, e := docker.New(ctx, dc)
			if e != nil {
				return fail(e)
			}
			out[name] = c
			locals[p.Socket] = local{dc, c}
			closers = append(closers, c.Close)
		default:
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
			out[name] = c
		}
	}
	return out, closeAll, nil
}
