package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/distribution/reference"
)

const (
	cachePath        = "/var/cache/pulse"
	cacheReadyFile   = "/nix/.pulse-ready"
	cacheNotReady    = 42
	cacheKeyLabel    = "pulse_cache_key"
	cacheOwnerLabel  = "pulse_cache_owner"
	cacheLayoutLabel = "pulse_cache_layout"
	roleLabel        = "pulse_resource_role"
	cacheInitRole    = "cache-init"
	cacheLayout      = "2"
)

// CacheConfig is comparable so Docker aliases must agree on cache policy too.
// Its zero value enables automatic, tenant-isolated caching of the Colony base.
type CacheConfig struct {
	Disabled              bool
	Namespace, Generation string
	MaxAge                time.Duration
}

func (c *CacheConfig) normalize() error {
	if c.Namespace == "" {
		c.Namespace = "default"
	}
	if c.Generation == "" {
		c.Generation = "1"
	}
	if c.MaxAge == 0 {
		c.MaxAge = 30 * 24 * time.Hour
	}
	if c.MaxAge < 0 || c.MaxAge > 365*24*time.Hour || len(c.Namespace) > 128 || len(c.Generation) > 128 || strings.ContainsAny(c.Namespace+c.Generation, "\x00\r\n") {
		return fmt.Errorf("invalid Docker dependency cache configuration")
	}
	return nil
}

type volumeMount struct {
	Type          string `json:"Type"`
	Source        string `json:"Source"`
	Target        string `json:"Target"`
	ReadOnly      bool   `json:"ReadOnly"`
	VolumeOptions struct {
		NoCopy bool `json:"NoCopy"`
	} `json:"VolumeOptions"`
}

type cacheVolume struct {
	Name, Driver, Scope, CreatedAt string
	Labels                         map[string]string
	Options                        map[string]string
}

type dependencyCache struct{ Key, Owner string }

func cacheHash(v any) string { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func (p *Provider) cacheOwner() string {
	return cacheHash([]string{cacheLayout, p.cfg.DependencyCache.Namespace})
}
func (c dependencyCache) volume(kind string) string { return "pulse-cache-v2-" + c.Key + "-" + kind }
func (c dependencyCache) initializer() string       { return "pulse-cache-init-" + c.Key }
func (c dependencyCache) labels(kind string) map[string]string {
	return map[string]string{cacheKeyLabel: c.Key, cacheOwnerLabel: c.Owner, cacheLayoutLabel: cacheLayout, roleLabel: kind}
}
func (c dependencyCache) mounts() []volumeMount {
	out := []volumeMount{{Type: "volume", Source: c.volume("tools"), Target: cachePath}, {Type: "volume", Source: c.volume("nix"), Target: "/nix"}}
	for i := range out {
		out[i].VolumeOptions.NoCopy = true
	}
	return out
}

func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == "/" || b == "/" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func cacheEnvironment(imageID string) map[string]string {
	root := cachePath
	if imageID != "" {
		root += "/images/" + cacheHash(imageID)
	}
	return map[string]string{"C2J_TOOL_CACHE_DIR": root + "/tools", "UV_CACHE_DIR": root + "/uv", "pnpm_config_store_dir": root + "/pnpm", "NIX_CACHE_HOME": root + "/nix", "NIX_REMOTE": "local", "UV_PYTHON_DOWNLOADS": "never"}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, s := range env {
		k, v, ok := strings.Cut(s, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}
func environmentContains(env []string, want map[string]string) bool {
	got := envMap(env)
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// NIX_CONFIG augments nix.conf. Preserve registry/signature policy and disable
// automatic GC: Nix's temporary roots assume a common PID namespace.
func cacheNixConfig(config string) (string, error) {
	for _, line := range strings.Split(config, "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "include" || fields[0] == "!include" {
			return "", fmt.Errorf("NIX_CONFIG includes are not supported with dependency caching")
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "min-free" && strings.TrimSpace(value) != "0" {
			return "", fmt.Errorf("dependency caching requires Nix min-free = 0")
		}
	}
	return strings.TrimSpace(config) + "\nmin-free = 0\n", nil
}

func (p *Provider) cachePlan(plan nativePlan, env map[string]string) (*dependencyCache, map[string]string, error) {
	if p.cfg.DependencyCache.Disabled {
		return nil, nil, nil
	}
	ref, err := reference.ParseNormalizedNamed(plan.Request.Image)
	if err != nil || ref.Name() != "ghcr.io/colony-2/base2" {
		return nil, nil, nil
	}
	switch plan.ImageUser {
	case "", "0", "0:0", "root", "root:root":
	default:
		return nil, nil, fmt.Errorf("Colony base dependency cache requires its root runtime user")
	}
	metadata := plan.Request.Metadata
	if metadata["pulse_jobdb_instance_id"] == "" || metadata["pulse_tenant_id"] == "" {
		return nil, nil, fmt.Errorf("dependency cache requires JobDB instance and tenant metadata")
	}
	values := cacheEnvironment(plan.ImageID)
	baseEnv := envMap(plan.ImageEnv)
	for k, v := range values {
		if supplied, ok := env[k]; ok && supplied != v {
			return nil, nil, fmt.Errorf("environment %s conflicts with Docker dependency cache", k)
		}
		if supplied, ok := baseEnv[k]; ok && supplied != v {
			return nil, nil, fmt.Errorf("image environment %s conflicts with Docker dependency cache", k)
		}
	}
	nixConfig := baseEnv["NIX_CONFIG"]
	if v, ok := env["NIX_CONFIG"]; ok {
		nixConfig = v
	}
	values["NIX_CONFIG"], err = cacheNixConfig(nixConfig)
	if err != nil {
		return nil, nil, err
	}
	key := cacheHash([]string{cacheLayout, p.cfg.DependencyCache.Namespace, p.cfg.DependencyCache.Generation, metadata["pulse_jobdb_instance_id"], metadata["pulse_tenant_id"], plan.Request.Platform, "0:0"})
	return &dependencyCache{Key: key, Owner: p.cacheOwner()}, values, nil
}

var cacheKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validInitializer(c container) bool {
	key := c.Config.Labels[cacheKeyLabel]
	return cacheKeyPattern.MatchString(key) && c.Config.Labels[roleLabel] == cacheInitRole && (c.Config.Labels[cacheLayoutLabel] == cacheLayout || c.Config.Labels[cacheLayoutLabel] == "1") && c.Config.Labels["pulse_launch_id"] == "cache-init-"+key && strings.TrimPrefix(c.Name, "/") == "pulse-cache-init-"+key
}

func verifyCacheContainer(c container, key string) bool {
	imageID := c.Config.Image
	legacy := c.Config.Labels[cacheLayoutLabel] != cacheLayout
	if legacy {
		imageID = ""
	}
	wantEnv := cacheEnvironment(imageID)
	// Existing v1/v2 workers may predate fetch-cache sharing. New launches
	// additionally verify the complete planned environment before starting.
	if _, present := envMap(c.Config.Env)["NIX_CACHE_HOME"]; !present {
		delete(wantEnv, "NIX_CACHE_HOME")
	}
	if !cacheKeyPattern.MatchString(key) || !environmentContains(c.Config.Env, wantEnv) {
		return false
	}
	if !strings.HasSuffix(envMap(c.Config.Env)["NIX_CONFIG"], "\nmin-free = 0\n") {
		return false
	}
	cache := dependencyCache{Key: key}
	for _, want := range cache.mounts() {
		if legacy {
			want.Source = strings.Replace(want.Source, "pulse-cache-v2-", "pulse-cache-v1-", 1)
		}
		found := false
		for _, got := range c.Mounts {
			// Retain inspection support for workers and initializers from v1.
			if got.Destination == want.Target || (legacy && want.Target == "/nix" && got.Destination == "/pulse-cache-seed/nix" && validInitializer(c)) {
				found = got.Type == "volume" && got.Name == want.Source && got.RW
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func apiStatus(err error, code int) bool {
	var e *apiError
	return errors.As(err, &e) && e.code == code
}

func (p *Provider) inspectVolume(ctx context.Context, c dependencyCache, kind string) (*cacheVolume, error) {
	var v cacheVolume
	err := p.e.call(ctx, "GET", "/volumes/"+c.volume(kind), nil, &v)
	if apiStatus(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (p *Provider) inspectInitializer(ctx context.Context, c dependencyCache) (*container, error) {
	var init container
	err := p.e.call(ctx, "GET", "/containers/"+c.initializer()+"/json", nil, &init)
	if apiStatus(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !validInitializer(init) || init.Config.Labels[cacheOwnerLabel] != c.Owner {
		return nil, fmt.Errorf("cache initializer ownership differs")
	}
	return &init, nil
}

// Readiness lives in the volume, so pruning stopped containers cannot lose it.
// The provider admission gate serializes provisioning; one Pulse owns the host.
func (p *Provider) ensureCache(ctx context.Context, c dependencyCache, plan nativePlan, cost charge) error {
	if p.readyCaches[c.Key] {
		return nil
	}
	slog.Info("Docker dependency cache initializing", "cache", c.Key, "image_id", plan.ImageID)
	nix, err := p.inspectVolume(ctx, c, "nix")
	if err != nil {
		return err
	}
	if nix != nil {
		// Creating a missing tools volume is safe; it contains no readiness state.
		if err := p.createCacheVolume(ctx, c, "tools"); err != nil {
			return err
		}
		code, err := p.runCacheHelper(ctx, c, plan, cost, cacheCheckScript)
		if err != nil {
			return err
		}
		if code == 0 {
			p.readyCaches[c.Key] = true
			return nil
		}
		if code != cacheNotReady {
			return fmt.Errorf("cache readiness check failed (exit %d)", code)
		}
	}
	// Missing store or marker: discard the uninitialized store and all associated
	// prepared records. Never remove volumes referenced by workers.
	removed, err := p.removeCache(ctx, c)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("uninitialized cache is still referenced")
	}
	for _, kind := range []string{"tools", "nix"} {
		if err := p.createCacheVolume(ctx, c, kind); err != nil {
			return err
		}
	}
	code, err := p.runCacheHelper(ctx, c, plan, cost, cacheInitializeScript)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("base image cache initialization failed (exit %d)", code)
	}
	p.readyCaches[c.Key] = true
	return nil
}

func (p *Provider) createCacheVolume(ctx context.Context, c dependencyCache, kind string) error {
	return p.e.call(ctx, "POST", "/volumes/create", map[string]any{"Name": c.volume(kind), "Driver": "local", "Labels": c.labels(kind)}, nil)
}

// Helpers are temporary, resource-accounted containers without job input.
func (p *Provider) runCacheHelper(ctx context.Context, c dependencyCache, plan nativePlan, cost charge, script string) (exitCode int, result error) {
	// A previous Pulse may have stopped while this helper was running. Stop it
	// before checking the marker; successful initialization has already written it.
	init, err := p.inspectInitializer(ctx, c)
	if err != nil {
		return 0, err
	}
	if init != nil {
		if err := p.e.call(ctx, "DELETE", "/containers/"+init.ID+"?force=1", nil, nil); err != nil && !apiStatus(err, 404) {
			return 0, err
		}
		delete(p.uncertain, "cache-init-"+c.Key)
	}
	started := time.Now()
	stage := "create helper"
	defer func() {
		if result != nil {
			// Initializers contain no job input; API errors omit daemon messages.
			slog.Warn("Docker dependency cache initialization failed", "cache", c.Key, "stage", stage, "elapsed", time.Since(started), "error", result)
		}
	}()
	labels := c.labels(cacheInitRole)
	labels["pulse_managed_by"] = "pulse"
	labels["pulse_launch_id"] = "cache-init-" + c.Key
	accounting, _ := json.Marshal(cost)
	labels[accountingLabel] = string(accounting)
	labels[limitsLabel] = "enforced"
	cpu, memory := containerLimits(plan.Allocation)
	if p.unenforced {
		cpu, memory = 0, 0
		labels[limitsLabel] = "disabled"
	}
	values := cacheEnvironment(plan.ImageID)
	values["NIX_CONFIG"], err = cacheNixConfig(envMap(plan.ImageEnv)["NIX_CONFIG"])
	if err != nil {
		return 0, err
	}
	env := []string{}
	for k, v := range values {
		env = append(env, k+"="+v)
	}
	body := map[string]any{"Image": plan.ImageID, "Entrypoint": []string{"/bin/sh"}, "Cmd": []string{"-ec", script}, "Env": env, "Labels": labels, "HostConfig": map[string]any{"Mounts": c.mounts(), "NetworkMode": "none", "NanoCpus": cpu, "Memory": memory, "MemorySwap": memory, "RestartPolicy": map[string]string{"Name": "no"}, "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m", "max-file": "1"}}}}
	helperID := labels["pulse_launch_id"]
	p.uncertain[helperID] = cost
	complete := false
	defer func() {
		if complete {
			delete(p.uncertain, helperID)
			return
		}
		// Only this verified initializer can be killed. It has no lease and runs
		// no user work. Unknown cleanup retains its capacity reservation.
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		init, err := p.inspectInitializer(cleanup, c)
		if err != nil {
			return
		}
		if init == nil {
			// A timed-out create can still complete after this inspection.
			return
		}
		if err = p.e.call(cleanup, "DELETE", "/containers/"+init.ID+"?force=1", nil, nil); err == nil || apiStatus(err, 404) {
			delete(p.uncertain, helperID)
		}
	}()
	var created struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	stage = "create initializer"
	if err = p.e.call(ctx, "POST", "/containers/create?name="+c.initializer(), body, &created); err != nil {
		var api *apiError
		if errors.As(err, &api) && api.code >= 400 && api.code < 500 && api.code != 409 {
			delete(p.uncertain, helperID)
		}
		return 0, err
	}
	if created.ID == "" || len(created.Warnings) != 0 {
		return 0, fmt.Errorf("cache initializer creation could not be verified")
	}
	init, err = p.inspectInitializer(ctx, c)
	if err != nil {
		return 0, err
	}
	if init == nil || init.ID != created.ID || !verifyCacheContainer(*init, c.Key) || !environmentContains(init.Config.Env, values) || init.HostConfig.AutoRemove || init.HostConfig.RestartPolicy.Name != "no" || init.HostConfig.NanoCPUs != cpu || init.HostConfig.Memory != memory || init.HostConfig.MemorySwap != memory {
		return 0, fmt.Errorf("cache initializer configuration differs")
	}
	stage = "start initializer"
	if err = p.e.call(ctx, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return 0, err
	}
	// Poll with the submission deadline and report which phase timed out.
	stage = "check or initialize cache"
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		init, err = p.inspectInitializer(ctx, c)
		if err != nil {
			return 0, err
		}
		if init == nil {
			return 0, fmt.Errorf("cache initializer disappeared")
		}
		if init.State.Status == "exited" {
			if err := p.e.call(ctx, "DELETE", "/containers/"+init.ID, nil, nil); err != nil && !apiStatus(err, 404) {
				return 0, err
			}
			complete = true
			slog.Info("Docker dependency cache helper finished", "cache", c.Key, "exit_code", init.State.ExitCode, "elapsed", time.Since(started))
			return init.State.ExitCode, nil
		}
		if init.State.Status == "dead" {
			return 0, fmt.Errorf("cache initializer died")
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

const cacheDirectoriesScript = `umask 077
mkdir -p "$C2J_TOOL_CACHE_DIR" "$UV_CACHE_DIR" "$pnpm_config_store_dir" "$NIX_CACHE_HOME"
`

const cacheCheckScript = cacheDirectoriesScript + `test -f ` + cacheReadyFile + ` || exit 42
`

const cacheInitializeScript = cacheDirectoriesScript + `nix-store --init
touch ` + cacheReadyFile + `
`

// Remove only idle pairs, never workers. The marker is deleted with the store.
func (p *Provider) removeCache(ctx context.Context, c dependencyCache) (bool, error) {
	// An ambiguous create may not yet appear in Docker's container listing.
	// Conservatively defer eviction until all such reservations are resolved.
	if len(p.uncertain) != 0 {
		return false, nil
	}
	for _, kind := range []string{"tools", "nix"} {
		if _, err := p.inspectVolume(ctx, c, kind); err != nil {
			return false, err
		}
	}
	init, err := p.inspectInitializer(ctx, c)
	if err != nil {
		return false, err
	}
	filter, _ := json.Marshal(map[string][]string{"volume": {c.volume("tools"), c.volume("nix")}})
	var refs []struct {
		ID string `json:"Id"`
	}
	if err = p.e.call(ctx, "GET", "/containers/json?"+url.Values{"all": {"1"}, "filters": {string(filter)}}.Encode(), nil, &refs); err != nil {
		return false, err
	}
	for _, ref := range refs {
		if init == nil || ref.ID != init.ID || (init.State.Status != "exited" && init.State.Status != "dead" && init.State.Status != "created") {
			return false, nil
		}
	}
	if init != nil {
		if init.State.Status != "exited" && init.State.Status != "dead" && init.State.Status != "created" {
			return false, nil
		}
		if err = p.e.call(ctx, "DELETE", "/containers/"+init.ID, nil, nil); err != nil && !apiStatus(err, 404) {
			return false, err
		}
		delete(p.uncertain, "cache-init-"+c.Key)
	}
	delete(p.readyCaches, c.Key)
	for _, kind := range []string{"nix", "tools"} {
		if err = p.e.call(ctx, "DELETE", "/volumes/"+c.volume(kind), nil, nil); err != nil && !apiStatus(err, 404) {
			return false, err
		}
	}
	return true, nil
}

// Maintain is called at startup and by the controller's normal polling loop.
// It retires only expired, unreferenced namespaces owned by this configuration.
func (p *Provider) Maintain(ctx context.Context) error {
	if p.cfg.DependencyCache.Disabled {
		return nil
	}
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer p.release()
	if !p.lastMaintenance.IsZero() && time.Since(p.lastMaintenance) < time.Hour {
		return nil
	}
	filter, _ := json.Marshal(map[string][]string{"label": {cacheLayoutLabel + "=" + cacheLayout, cacheOwnerLabel + "=" + p.cacheOwner()}})
	var result struct{ Volumes []cacheVolume }
	if err := p.e.call(ctx, "GET", "/volumes?"+url.Values{"filters": {string(filter)}}.Encode(), nil, &result); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range result.Volumes {
		key := v.Labels[cacheKeyLabel]
		if seen[key] {
			continue
		}
		seen[key] = true
		if !cacheKeyPattern.MatchString(key) || v.Labels[cacheOwnerLabel] != p.cacheOwner() || v.Labels[cacheLayoutLabel] != cacheLayout {
			return fmt.Errorf("invalid cache ownership in volume listing")
		}
		c := dependencyCache{Key: key, Owner: p.cacheOwner()}
		if v.Name != c.volume("tools") && v.Name != c.volume("nix") {
			return fmt.Errorf("cache volume name differs")
		}
		created, err := time.Parse(time.RFC3339Nano, v.CreatedAt)
		if err != nil {
			return fmt.Errorf("cache volume creation time unavailable")
		}
		if time.Since(created) < p.cfg.DependencyCache.MaxAge {
			continue
		}
		removed, err := p.removeCache(ctx, c)
		if err != nil {
			return err
		}
		if removed {
			slog.Info("retired idle Docker dependency cache", "cache", key)
		}
	}
	p.lastMaintenance = time.Now()
	return nil
}
