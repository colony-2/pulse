// Package docker admits batches against committed Docker allocations, not usage.
package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
)

const accountingLabel = "pulse_docker_accounting"

type Config struct {
	Socket, Helper, LockDir, ScratchPath, RegistryAuth string
	CPUMillis, MemoryBytes, Overhead                   int64
	MaxContainers                                      int
}
type charge struct {
	CPU    int64 `json:"cpu"`
	Memory int64 `json:"memory"`
	Slots  int   `json:"slots"`
}

func (c charge) add(b charge) charge {
	return charge{c.CPU + b.CPU, c.Memory + b.Memory, c.Slots + b.Slots}
}

type Provider struct {
	cfg       Config
	e         *engine
	platform  string
	gate      chan struct{}
	uncertain map[string]charge
	lock      *os.File
	closeOnce sync.Once
}
type nativePlan struct {
	Request    compute.Request
	ImageID    string
	Allocation compute.Allocation
}
type hostConfig struct {
	NanoCPUs      int64 `json:"NanoCpus"`
	Memory        int64 `json:"Memory"`
	MemorySwap    int64 `json:"MemorySwap"`
	RestartPolicy struct {
		Name string `json:"Name"`
	} `json:"RestartPolicy"`
	Tmpfs map[string]string `json:"Tmpfs"`
}
type container struct {
	HostConfig hostConfig `json:"HostConfig"`
	ID         string     `json:"Id"`
	Name       string     `json:"Name"`
	Config     struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status    string `json:"Status"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
}

func New(ctx context.Context, cfg Config) (*Provider, error) {
	if runtime.GOOS != "linux" || !strings.HasPrefix(cfg.Socket, "unix:///") {
		return nil, fmt.Errorf("local Docker requires Linux and a Unix socket")
	}
	if !filepath.IsAbs(cfg.Helper) {
		return nil, fmt.Errorf("Docker helper must be an absolute daemon-visible path")
	}
	stat, err := os.Stat(cfg.Helper)
	if err != nil || stat.IsDir() || stat.Mode()&0111 == 0 {
		return nil, fmt.Errorf("Docker helper is missing or not executable")
	}
	e := newEngine(cfg.Socket)
	e.registryAuth = cfg.RegistryAuth
	return open(ctx, cfg, e, true)
}
func open(ctx context.Context, cfg Config, e *engine, locking bool) (*Provider, error) {
	if cfg.CPUMillis <= 0 || cfg.MemoryBytes <= 0 || cfg.MaxContainers <= 0 || cfg.Overhead < 0 {
		return nil, fmt.Errorf("invalid Docker capacity budget")
	}
	if cfg.ScratchPath == "" {
		cfg.ScratchPath = "/scratch"
	}
	if !filepath.IsAbs(cfg.ScratchPath) || cfg.ScratchPath == "/" || strings.Contains(cfg.ScratchPath, ":") {
		return nil, fmt.Errorf("invalid scratch path")
	}
	var version struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := e.call(ctx, "GET", "/version", nil, &version); err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^1\.[0-9]+$`).MatchString(version.APIVersion) {
		return nil, fmt.Errorf("invalid Docker API version")
	}
	e.version = "/v" + version.APIVersion
	var info struct {
		ID                                          string
		NCPU                                        int
		MemTotal                                    int64
		OSType, Architecture                        string
		MemoryLimit, SwapLimit, CPUSet, CPUCfsQuota bool
	}
	if err := e.call(ctx, "GET", "/info", nil, &info); err != nil {
		return nil, err
	}
	arch := info.Architecture
	switch arch {
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}
	if info.OSType != "linux" || !info.MemoryLimit || !info.SwapLimit || !info.CPUCfsQuota {
		return nil, fmt.Errorf("Docker daemon lacks required Linux CPU/memory/swap limits")
	}
	if cfg.CPUMillis > int64(info.NCPU)*1000 || cfg.MemoryBytes > info.MemTotal {
		return nil, fmt.Errorf("Docker budget exceeds daemon resources")
	}
	p := &Provider{cfg: cfg, e: e, platform: info.OSType + "/" + arch, gate: make(chan struct{}, 1), uncertain: map[string]charge{}}
	if locking {
		if cfg.LockDir == "" {
			cfg.LockDir = filepath.Join(os.TempDir(), "pulse-docker-locks")
		}
		if err := os.MkdirAll(cfg.LockDir, 0700); err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(info.ID))
		f, err := os.OpenFile(filepath.Join(cfg.LockDir, hex.EncodeToString(sum[:])+".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return nil, fmt.Errorf("Docker daemon already has an admission owner")
		}
		p.lock = f
	}
	if _, _, err := p.usage(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}
func (p *Provider) Close() {
	p.closeOnce.Do(func() {
		if p.lock != nil {
			syscall.Flock(int(p.lock.Fd()), syscall.LOCK_UN)
			p.lock.Close()
		}
	})
}
func (p *Provider) acquire(ctx context.Context) error {
	select {
	case p.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *Provider) release() { <-p.gate }
func (p *Provider) fits(c charge) bool {
	return c.CPU <= p.cfg.CPUMillis && c.Memory <= p.cfg.MemoryBytes && c.Slots <= p.cfg.MaxContainers
}
func (p *Provider) cost(a compute.Allocation) charge {
	return charge{a.CPUMillis, a.MemoryBytes + a.ScratchBytes + p.cfg.Overhead, 1}
}
func (p *Provider) usage(ctx context.Context) (charge, map[string]container, error) {
	filter, _ := json.Marshal(map[string][]string{"label": {"pulse_managed_by=pulse"}})
	q := url.Values{"all": {"1"}, "filters": {string(filter)}}
	var list []struct {
		ID string `json:"Id"`
	}
	if err := p.e.call(ctx, "GET", "/containers/json?"+q.Encode(), nil, &list); err != nil {
		return charge{}, nil, err
	}
	total := charge{}
	found := map[string]container{}
	for _, item := range list {
		var c container
		if err := p.e.call(ctx, "GET", "/containers/"+url.PathEscape(item.ID)+"/json", nil, &c); err != nil {
			return charge{}, nil, err
		}
		id := c.Config.Labels["pulse_launch_id"]
		if id == "" {
			return charge{}, nil, fmt.Errorf("managed container lacks launch ID")
		}
		if _, ok := found[id]; ok {
			return charge{}, nil, fmt.Errorf("duplicate managed launch ID")
		}
		found[id] = c
		var cost charge
		if err := json.Unmarshal([]byte(c.Config.Labels[accountingLabel]), &cost); err != nil || cost.CPU <= 0 || cost.Memory <= 0 || cost.Slots != 1 {
			return charge{}, nil, fmt.Errorf("invalid managed capacity accounting")
		}
		h := c.HostConfig
		if h.NanoCPUs <= 0 || h.NanoCPUs%1000000 != 0 || h.NanoCPUs/1000000 != cost.CPU || h.Memory <= 0 || h.Memory > cost.Memory || h.MemorySwap != h.Memory || h.RestartPolicy.Name != "no" {
			return charge{}, nil, fmt.Errorf("managed container limits disagree with accounting")
		}
		delete(p.uncertain, id)
		switch c.State.Status {
		case "exited", "dead":
		default:
			total = total.add(cost)
		}
	}
	for _, cost := range p.uncertain {
		total = total.add(cost)
	}
	return total, found, nil
}
func (p *Provider) resolve(ctx context.Context, r compute.Request, used charge) (nativePlan, compute.Status, string) {
	result := compute.Submission{}
	a := r.Allocation
	a.CPUMillis = (a.CPUMillis + 9) / 10 * 10
	a.MemoryBytes = (a.MemoryBytes + 4095) / 4096 * 4096
	a.ScratchBytes = (a.ScratchBytes + 4095) / 4096 * 4096
	if a.MemoryBytes+a.ScratchBytes < 6<<20 {
		a.MemoryBytes = (6 << 20) - a.ScratchBytes
	}
	cost := p.cost(a)
	switch {
	case r.Platform != p.platform:
		result.Status = compute.Unsupported
		result.Reason = "Docker native platform differs"
	case !p.fits(cost):
		result.Status = compute.Unsupported
		result.Reason = "request exceeds Docker pool budget"
	case !p.fits(used.add(cost)):
		result.Status = compute.NoCapacity
		result.Reason = "Docker pool full"
	default:
		var image struct {
			ID           string   `json:"Id"`
			RepoDigests  []string `json:"RepoDigests"`
			OS           string   `json:"Os"`
			Architecture string   `json:"Architecture"`
		}
		imagePath := "/images/" + url.PathEscape(r.Image) + "/json"
		e := p.e.call(ctx, "GET", imagePath, nil, &image)
		var ae *apiError
		if errors.As(e, &ae) && ae.code == 404 {
			e = p.e.pull(ctx, r.Image, r.Platform)
			if e == nil {
				e = p.e.call(ctx, "GET", imagePath, nil, &image)
			}
		}
		if e != nil {
			result.Status = compute.Unavailable
			result.Reason = "Docker image resolution failed"
			return nativePlan{}, result.Status, result.Reason
		}
		if image.ID == "" || image.OS+"/"+image.Architecture != r.Platform {
			result.Status = compute.Unsupported
			result.Reason = "image platform differs"
			return nativePlan{}, result.Status, result.Reason
		}
		a.ImageID = image.ID
		a.ImageDigest = ""
		if _, digest, pinned := strings.Cut(r.Image, "@"); pinned {
			for _, rd := range image.RepoDigests {
				if rd == r.Image {
					a.ImageDigest = digest
				}
			}
			if a.ImageDigest == "" {
				result.Status = compute.Unsupported
				result.Reason = "required manifest identity is unverified"
				return nativePlan{}, result.Status, result.Reason
			}
		}
		return nativePlan{Request: r, ImageID: image.ID, Allocation: a}, "", ""
	}
	return nativePlan{}, result.Status, result.Reason
}

func fingerprint(l compute.Launch) string {
	b, _ := json.Marshal(l)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p *Provider) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	if err := compute.ValidateLaunches(ls); err != nil {
		return nil, err
	}
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()
	used, found, err := p.usage(ctx)
	if err != nil {
		out := []compute.Submission{}
		for _, l := range ls {
			out = append(out, compute.Submission{LaunchID: l.LaunchID, Status: compute.Unavailable, Reason: "Docker accounting unavailable before submission"})
		}
		return out, err
	}
	out := []compute.Submission{}
	for _, l := range ls {
		r := compute.Submission{LaunchID: l.LaunchID}
		hash := fingerprint(l)
		if c, exists := found[l.LaunchID]; exists {
			if c.Config.Labels["pulse_process_fingerprint"] != hash {
				r.Status = compute.Rejected
				r.Reason = "launch ID conflict"
			} else if c.State.Status == "created" {
				r.Status = compute.Unknown
				r.Reason = "previous launch was created but start is unconfirmed"
			} else {
				r.Status = compute.Accepted
			}
			out = append(out, r)
			continue
		}
		if _, uncertain := p.uncertain[l.LaunchID]; uncertain {
			r.Status = compute.Unknown
			r.Reason = "previous Docker create remains uncertain"
			out = append(out, r)
			continue
		}

		plan, status, reason := p.resolve(ctx, l.Request, used)
		if status != "" {
			r.Status, r.Reason = status, reason
			out = append(out, r)
			continue
		}
		cost := p.cost(plan.Allocation)

		if plan.Request.StartBefore != nil && !plan.Request.StartBefore.After(time.Now()) {
			r.Status = compute.Rejected
			r.Reason = "start deadline expired"
			out = append(out, r)
			continue
		}
		used = used.add(cost)
		p.uncertain[l.LaunchID] = cost
		labels := map[string]string{}
		for k, v := range plan.Request.Metadata {
			labels[k] = v
		}
		labels["pulse_managed_by"] = "pulse"
		labels["pulse_launch_id"] = l.LaunchID
		accounting, _ := json.Marshal(cost)
		labels[accountingLabel] = string(accounting)
		labels["pulse_process_fingerprint"] = hash
		env := []string{}
		for k, v := range l.Process.Env {
			env = append(env, k+"="+v)
		}
		if _, ok := l.Process.Env["TMPDIR"]; !ok {
			env = append(env, "TMPDIR="+p.cfg.ScratchPath)
		}
		if _, supplied := l.Process.Env["PULSE_SCRATCH_DIR"]; !supplied {
			env = append(env, "PULSE_SCRATCH_DIR="+p.cfg.ScratchPath)
		}
		args := []string{"--timeout", strconv.FormatInt(plan.Request.TimeoutSeconds, 10) + "s"}
		if plan.Request.StartBefore != nil {
			args = append(args, "--start-before", plan.Request.StartBefore.Format(time.RFC3339Nano))
		}
		args = append(args, "--")
		args = append(args, l.Process.Command...)
		args = append(args, l.Process.Args...)
		a := plan.Allocation
		body := map[string]any{"Image": plan.ImageID, "Entrypoint": []string{"/__pulse/exec"}, "Cmd": args, "Env": env, "Labels": labels, "WorkingDir": l.Process.WorkingDir, "HostConfig": map[string]any{"NanoCpus": a.CPUMillis * 1000000, "Memory": a.MemoryBytes + a.ScratchBytes, "MemorySwap": a.MemoryBytes + a.ScratchBytes, "RestartPolicy": map[string]any{"Name": "no"}, "AutoRemove": false, "Binds": []string{p.cfg.Helper + ":/__pulse/exec:ro"}, "Tmpfs": map[string]string{p.cfg.ScratchPath: fmt.Sprintf("rw,size=%d,mode=1777", a.ScratchBytes)}, "LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "3"}}}}
		sum := sha256.Sum256([]byte(l.LaunchID))
		name := "pulse-" + hex.EncodeToString(sum[:16])
		var created struct {
			ID       string   `json:"Id"`
			Warnings []string `json:"Warnings"`
		}
		err = p.e.call(ctx, "POST", "/containers/create?name="+name, body, &created)
		if err != nil {
			var api *apiError
			if errors.As(err, &api) && api.code >= 400 && api.code < 500 && api.code != 409 {
				delete(p.uncertain, l.LaunchID)
				used.CPU -= cost.CPU
				used.Memory -= cost.Memory
				used.Slots--
				r.Status = compute.Rejected
				r.Reason = err.Error()
			} else {
				r.Status = compute.Unknown
				r.Reason = "Docker create outcome uncertain"
			}
			out = append(out, r)
			continue
		}
		delete(p.uncertain, l.LaunchID)
		if created.ID == "" {
			p.uncertain[l.LaunchID] = cost
			r.Status = compute.Unknown
			r.Reason = "Docker create returned no ID"
			out = append(out, r)
			continue
		}
		if len(created.Warnings) > 0 {
			r.Status = compute.Unknown
			r.Reason = "Docker create warnings require inspection; container not started"
			out = append(out, r)
			continue
		}
		var inspected container
		if err = p.e.call(ctx, "GET", "/containers/"+url.PathEscape(created.ID)+"/json", nil, &inspected); err != nil || inspected.HostConfig.NanoCPUs != a.CPUMillis*1000000 || inspected.HostConfig.Memory != a.MemoryBytes+a.ScratchBytes || inspected.HostConfig.MemorySwap != inspected.HostConfig.Memory || inspected.HostConfig.RestartPolicy.Name != "no" || inspected.HostConfig.Tmpfs[p.cfg.ScratchPath] != fmt.Sprintf("rw,size=%d,mode=1777", a.ScratchBytes) {
			r.Status = compute.Unknown
			r.Reason = "Docker configuration verification failed; container not started"
			out = append(out, r)
			continue
		}
		if err = p.e.call(ctx, "POST", "/containers/"+url.PathEscape(created.ID)+"/start", nil, nil); err != nil {
			r.Status = compute.Unknown
			r.Reason = "Docker start outcome uncertain"
		} else {
			r.Status = compute.Accepted
		}
		out = append(out, r)
	}
	return out, nil
}
