package providers

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	docker "github.com/colony-2/pulse-docker"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providerutil"
	"github.com/colony-2/pulse/internal/quantity"
)

// Normalize host defaults before comparing aliases. Discovery budgets remain
// zero: an explicit budget is a different policy even if it matches the daemon.
func dockerSettings(p config.Provider, cfg *config.Config) (docker.Config, error) {
	c := docker.Config{
		LockDir: p.LockDir, ScratchPath: p.ScratchPath,
		RegistryAuth: os.Getenv(p.RegistryAuthEnv), Overhead: 256 << 20,
		MaxContainers: p.Capacity.MaxContainers, ExecutorEnv: maps.Clone(cfg.Defaults.Env),
	}
	var err error
	c.Socket, err = docker.ResolveSocket(p.Socket)
	if err != nil {
		return c, err
	}
	for _, field := range []struct {
		value string
		cpu   bool
		out   *int64
	}{{p.Capacity.CPU, true, &c.CPUMillis}, {p.Capacity.Memory, false, &c.MemoryBytes}} {
		if field.value != "" {
			*field.out, err = quantity.Parse(field.value, field.cpu)
			if err != nil {
				return c, err
			}
		}
	}
	if p.Overhead != "" {
		c.Overhead, err = quantity.NonnegativeBytes(p.Overhead)
		if err != nil {
			return c, err
		}
	}
	if c.LockDir == "" {
		c.LockDir = filepath.Join(os.TempDir(), "pulse-docker-locks")
	}
	if c.ScratchPath == "" {
		c.ScratchPath = "/scratch"
	}
	if cache := p.DependencyCache; cache != nil {
		c.DependencyCache = docker.CacheConfig{Disabled: cache.Enabled != nil && !*cache.Enabled, Namespace: cache.Namespace, Generation: cache.Generation}
		if cache.MaxAge != "" {
			c.DependencyCache.MaxAge, err = time.ParseDuration(cache.MaxAge)
			if err != nil {
				return c, fmt.Errorf("invalid Docker dependency cache duration")
			}
		}
	}
	if c.DependencyCache.Namespace == "" {
		c.DependencyCache.Namespace = "default"
	}
	if c.DependencyCache.Generation == "" {
		c.DependencyCache.Generation = "1"
	}
	if c.DependencyCache.MaxAge == 0 {
		c.DependencyCache.MaxAge = 30 * 24 * time.Hour
	}
	seen := map[string]string{}
	for _, target := range cfg.Targets {
		e := docker.LegacyEndpoint{InstanceID: target.Instance, TenantID: target.Tenant, Endpoint: target.JobDB}
		if providerutil.Job(api.JobRef{InstanceID: e.InstanceID, TenantID: e.TenantID, JobDB: e.Endpoint, JobID: "validation"}) != nil {
			return c, fmt.Errorf("invalid Docker legacy endpoint")
		}
		key := e.InstanceID + "\x00" + e.TenantID
		if prior, ok := seen[key]; ok {
			if prior != e.Endpoint {
				return c, fmt.Errorf("conflicting Docker legacy endpoints")
			}
			continue
		}
		seen[key] = e.Endpoint
		c.LegacyEndpoints = append(c.LegacyEndpoints, e)
	}
	slices.SortFunc(c.LegacyEndpoints, func(a, b docker.LegacyEndpoint) int {
		if n := cmp.Compare(a.InstanceID, b.InstanceID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.TenantID, b.TenantID); n != 0 {
			return n
		}
		return cmp.Compare(a.Endpoint, b.Endpoint)
	})
	return c, nil
}

// Compare every host setting explicitly, with collections compared by content.
// Authorization participates in equality but must never appear in an error.
func sameDockerSettings(a, b docker.Config) bool {
	return a.Socket == b.Socket && a.LockDir == b.LockDir &&
		a.ScratchPath == b.ScratchPath && a.RegistryAuth == b.RegistryAuth &&
		a.CPUMillis == b.CPUMillis && a.MemoryBytes == b.MemoryBytes &&
		a.Overhead == b.Overhead && a.MaxContainers == b.MaxContainers &&
		a.DependencyCache.Disabled == b.DependencyCache.Disabled &&
		a.DependencyCache.Namespace == b.DependencyCache.Namespace &&
		a.DependencyCache.Generation == b.DependencyCache.Generation &&
		a.DependencyCache.MaxAge == b.DependencyCache.MaxAge &&
		maps.Equal(a.ExecutorEnv, b.ExecutorEnv) &&
		slices.EqualFunc(a.LegacyEndpoints, b.LegacyEndpoints, func(a, b docker.LegacyEndpoint) bool {
			return a.InstanceID == b.InstanceID && a.TenantID == b.TenantID && a.Endpoint == b.Endpoint
		})
}
