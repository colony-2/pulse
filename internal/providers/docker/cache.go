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
	cacheKeyLabel    = "pulse_cache_key"
	cacheOwnerLabel  = "pulse_cache_owner"
	cacheLayoutLabel = "pulse_cache_layout"
	roleLabel        = "pulse_resource_role"
	cacheInitRole    = "cache-init"
	cacheLayout      = "1"
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
func (c dependencyCache) volume(kind string) string { return "pulse-cache-v1-" + c.Key + "-" + kind }
func (c dependencyCache) initializer() string       { return "pulse-cache-init-" + c.Key }
func (c dependencyCache) labels(kind string) map[string]string {
	return map[string]string{cacheKeyLabel: c.Key, cacheOwnerLabel: c.Owner, cacheLayoutLabel: cacheLayout, roleLabel: kind}
}
func (c dependencyCache) mounts(noCopy bool) []volumeMount {
	out := []volumeMount{{Type: "volume", Source: c.volume("tools"), Target: cachePath}, {Type: "volume", Source: c.volume("nix"), Target: "/nix"}}
	for i := range out {
		out[i].VolumeOptions.NoCopy = noCopy || out[i].Target == cachePath
	}
	return out
}

func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == "/" || b == "/" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func cacheEnvironment() map[string]string {
	return map[string]string{"C2J_TOOL_CACHE_DIR": cachePath + "/tools", "UV_CACHE_DIR": cachePath + "/uv", "pnpm_config_store_dir": cachePath + "/pnpm", "NIX_REMOTE": "local", "UV_PYTHON_DOWNLOADS": "never"}
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
	if err != nil || ref.Name() != "ghcr.io/colony-2/base" {
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
	values := cacheEnvironment()
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
	key := cacheHash([]string{cacheLayout, p.cfg.DependencyCache.Namespace, p.cfg.DependencyCache.Generation, metadata["pulse_jobdb_instance_id"], metadata["pulse_tenant_id"], plan.ImageID, plan.Request.Platform, "0:0"})
	return &dependencyCache{Key: key, Owner: p.cacheOwner()}, values, nil
}

var cacheKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validInitializer(c container) bool {
	key := c.Config.Labels[cacheKeyLabel]
	return cacheKeyPattern.MatchString(key) && c.Config.Labels[roleLabel] == cacheInitRole && c.Config.Labels[cacheLayoutLabel] == cacheLayout && c.Config.Labels["pulse_launch_id"] == "cache-init-"+key && strings.TrimPrefix(c.Name, "/") == "pulse-cache-init-"+key
}

func verifyCacheContainer(c container, key string) bool {
	if !cacheKeyPattern.MatchString(key) || !environmentContains(c.Config.Env, cacheEnvironment()) {
		return false
	}
	if !strings.HasSuffix(envMap(c.Config.Env)["NIX_CONFIG"], "\nmin-free = 0\n") {
		return false
	}
	cache := dependencyCache{Key: key}
	for _, want := range cache.mounts(true) {
		found := false
		for _, got := range c.Mounts {
			if got.Destination == want.Target {
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
	if v.Name != c.volume(kind) || v.Driver != "local" || v.Scope != "local" || len(v.Options) != 0 {
		return nil, fmt.Errorf("cache volume driver or identity differs")
	}
	for k, want := range c.labels(kind) {
		if v.Labels[k] != want {
			return nil, fmt.Errorf("cache volume ownership differs")
		}
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

// Keep the successful, stopped initializer as the durable readiness record.
// Docker inspection then provides warm readiness and its completion timestamp
// without a helper process on every launch or a controller-side database.
func (p *Provider) ensureCache(ctx context.Context, c dependencyCache, plan nativePlan, cost charge) (result error) {
	tools, err := p.inspectVolume(ctx, c, "tools")
	if err != nil {
		return err
	}
	nix, err := p.inspectVolume(ctx, c, "nix")
	if err != nil {
		return err
	}
	init, err := p.inspectInitializer(ctx, c)
	if err != nil {
		return err
	}
	if init != nil && tools != nil && nix != nil && init.State.Status == "exited" && init.State.ExitCode == 0 && init.Config.Image == plan.ImageID && verifyCacheContainer(*init, c.Key) {
		return nil
	}
	if init != nil || tools != nil || nix != nil {
		removed, err := p.removeCache(ctx, c)
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("incomplete cache is still referenced")
		}
	}
	started := time.Now()
	stage := "create volumes"
	slog.Info("Docker dependency cache initializing", "cache", c.Key, "image_id", plan.ImageID)
	defer func() {
		if result != nil {
			// Initializers contain no job input; API errors omit daemon messages.
			slog.Warn("Docker dependency cache initialization failed", "cache", c.Key, "stage", stage, "elapsed", time.Since(started), "error", result)
		}
	}()
	for _, kind := range []string{"tools", "nix"} {
		if err := p.e.call(ctx, "POST", "/volumes/create", map[string]any{"Name": c.volume(kind), "Driver": "local", "Labels": c.labels(kind)}, nil); err != nil {
			return err
		}
		if v, err := p.inspectVolume(ctx, c, kind); err != nil {
			return err
		} else if v == nil {
			return fmt.Errorf("cache volume was not created")
		}
	}
	labels := c.labels(cacheInitRole)
	labels["pulse_managed_by"] = "pulse"
	labels["pulse_launch_id"] = "cache-init-" + c.Key
	accounting, _ := json.Marshal(cost)
	labels[accountingLabel] = string(accounting)
	labels[limitsLabel] = "enforced"
	cpu, memory := plan.Allocation.CPUMillis*1000000, plan.Allocation.MemoryBytes+plan.Allocation.ScratchBytes
	if p.unenforced {
		cpu, memory = 0, 0
		labels[limitsLabel] = "disabled"
	}
	values := cacheEnvironment()
	values["NIX_CONFIG"], err = cacheNixConfig(envMap(plan.ImageEnv)["NIX_CONFIG"])
	if err != nil {
		return err
	}
	env := []string{}
	for k, v := range values {
		env = append(env, k+"="+v)
	}
	body := map[string]any{"Image": plan.ImageID, "Entrypoint": []string{"/bin/sh"}, "Cmd": []string{"-ec", cacheInitializeScript}, "Env": env, "Labels": labels, "HostConfig": map[string]any{"Mounts": c.mounts(false), "NetworkMode": "none", "NanoCpus": cpu, "Memory": memory, "MemorySwap": memory, "RestartPolicy": map[string]string{"Name": "no"}, "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "1m", "max-file": "1"}}}}
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
	stage = "create initializer (Nix volume copy-up)"
	if err = p.e.call(ctx, "POST", "/containers/create?name="+c.initializer(), body, &created); err != nil {
		var api *apiError
		if errors.As(err, &api) && api.code >= 400 && api.code < 500 && api.code != 409 {
			delete(p.uncertain, helperID)
		}
		return err
	}
	if created.ID == "" || len(created.Warnings) != 0 {
		return fmt.Errorf("cache initializer creation could not be verified")
	}
	init, err = p.inspectInitializer(ctx, c)
	if err != nil {
		return err
	}
	if init == nil || init.ID != created.ID || !verifyCacheContainer(*init, c.Key) || init.HostConfig.AutoRemove || init.HostConfig.RestartPolicy.Name != "no" || init.HostConfig.NanoCPUs != cpu || init.HostConfig.Memory != memory || init.HostConfig.MemorySwap != memory {
		return fmt.Errorf("cache initializer configuration differs")
	}
	stage = "start initializer"
	if err = p.e.call(ctx, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return err
	}
	// Poll with the submission deadline and report which phase timed out.
	stage = "validate initialized cache"
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		init, err = p.inspectInitializer(ctx, c)
		if err != nil {
			return err
		}
		if init == nil {
			return fmt.Errorf("cache initializer disappeared")
		}
		if init.State.Status == "exited" {
			if init.State.ExitCode != 0 {
				return fmt.Errorf("base image cache validation failed (exit %d)", init.State.ExitCode)
			}
			complete = true
			slog.Info("Docker dependency cache initialized", "cache", c.Key, "image_id", plan.ImageID)
			return nil
		}
		if init.State.Status == "dead" {
			return fmt.Errorf("cache initializer died")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

const cacheInitializeScript = `umask 077
mkdir -p /var/cache/pulse/tools /var/cache/pulse/uv /var/cache/pulse/pnpm
for tool in c2j nix uv pnpm node python3 git; do command -v "$tool" >/dev/null; done
nix-store --check-validity "$(readlink -f /root/.nix-profile)"
test "$(uv cache dir)" = "$UV_CACHE_DIR"
case "$(pnpm store path)" in "$pnpm_config_store_dir"/*) ;; *) exit 1;; esac
python3 -c 'import os; assert os.getuid() == 0; assert os.access("/nix/var/nix/db", os.W_OK)'
`

// removeCache runs under admission ownership. A stopped initializer is both
// the readiness record and a reference protecting the volumes from pruning.
// Remove it first: after a crash, leftover volumes have no readiness record and
// are reconciled before reuse. Never force volume removal or delete workers.
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
		init, err := p.inspectInitializer(ctx, c)
		if err != nil {
			return err
		}
		if init != nil && init.State.Status == "exited" && init.State.ExitCode == 0 {
			created, err = time.Parse(time.RFC3339Nano, init.State.FinishedAt)
			if err != nil {
				return fmt.Errorf("cache initialization time unavailable")
			}
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
