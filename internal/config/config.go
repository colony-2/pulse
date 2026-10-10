package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/quantity"
	"github.com/colony-2/pulse/pkg/compute"
	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Service struct {
	Name     string `yaml:"name"`
	Priority int    `yaml:"priority"`
}
type Target struct {
	JobDBTokenEnv string    `yaml:"jobdb_token_env"`
	Instance      string    `yaml:"instance_id"`
	JobDB         string    `yaml:"jobdb"`
	Services      []Service `yaml:"launch_services"`
	Tenant        string    `yaml:"-"`
}
type Provider struct {
	DependencyCache    *DependencyCache  `yaml:"dependency_cache"`
	SupervisorPath     string            `yaml:"supervisor_path"`
	ImageStorageBounds map[string]string `yaml:"image_storage_bounds"`
	MaxAzureCPU        int64             `yaml:"max_azure_cpu_millis"`

	Type      string `yaml:"type"`
	Endpoint  string `yaml:"endpoint"`
	TokenEnv  string `yaml:"token_env"`
	AllowHTTP bool   `yaml:"allow_http"`
	Socket    string `yaml:"socket"`
	LockDir   string `yaml:"lock_dir"`
	Capacity  struct {
		CPU           string `yaml:"cpu"`
		Memory        string `yaml:"memory"`
		MaxContainers int    `yaml:"max_containers"`
	} `yaml:"capacity"`
	Overhead        string   `yaml:"per_container_memory_overhead"`
	ScratchPath     string   `yaml:"scratch_path"`
	RegistryAuthEnv string   `yaml:"registry_auth_env"`
	Project         string   `yaml:"project"`
	Region          string   `yaml:"region"`
	Cluster         string   `yaml:"cluster"`
	Subnets         []string `yaml:"subnets"`
	SecurityGroups  []string `yaml:"security_groups"`
	PublicIP        bool     `yaml:"public_ip"`
	ExecutionRole   string   `yaml:"execution_role"`
	TaskRole        string   `yaml:"task_role"`
	Subscription    string   `yaml:"subscription"`
	ResourceGroup   string   `yaml:"resource_group"`
	EnvironmentID   string   `yaml:"environment_id"`
	ServiceAccount  string   `yaml:"service_account"`
	Command         string   `yaml:"command"`
}

type DependencyCache struct {
	Enabled    *bool  `yaml:"enabled"`
	Namespace  string `yaml:"namespace"`
	Generation string `yaml:"generation"`
	MaxAge     string `yaml:"max_age"`
}
type Config struct {
	HTTP struct {
		Listen string `yaml:"listen"`
	} `yaml:"http"`
	PollInterval     string `yaml:"poll_interval"`
	Cooldown         string `yaml:"cooldown"`
	LeaseDuration    string `yaml:"lease_duration"`
	CallTimeout      string `yaml:"call_timeout"`
	BatchTimeout     string `yaml:"batch_timeout"`
	ClaimTimeout     string `yaml:"claim_timeout"`
	ClaimConcurrency int    `yaml:"claim_concurrency"`
	BatchSize        int    `yaml:"batch_size"`
	PerTenant        int    `yaml:"max_jobs_per_tenant"`
	MaxPages         int    `yaml:"max_pages_per_tenant"`
	Defaults         struct {
		Image       string            `yaml:"image"`
		Platform    string            `yaml:"platform"`
		CPU         string            `yaml:"cpu"`
		Memory      string            `yaml:"memory"`
		Scratch     string            `yaml:"scratch"`
		Timeout     string            `yaml:"timeout"`
		StartWindow string            `yaml:"start_window"`
		Env         map[string]string `yaml:"env"`
		Routes      []c2j.Route       `yaml:"routes"`
	} `yaml:"defaults"`
	Providers                                              map[string]Provider `yaml:"providers"`
	Targets                                                []Target            `yaml:"targets"`
	Allocation                                             compute.Allocation  `yaml:"-"`
	AutoPlatform                                           bool                `yaml:"-"`
	Poll, Cool, Call, Batch, ExecutionTimeout, StartWindow time.Duration       `yaml:"-"`
	Claim                                                  time.Duration       `yaml:"-"`
	Lease                                                  time.Duration       `yaml:"-"`
}

func Load(path string) (*Config, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	return parse(context.Background(), b, "")
}

func parse(ctx context.Context, b []byte, jobdb string) (*Config, error) {
	c := &Config{}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if e := d.Decode(c); e != nil {
		return nil, e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return nil, fmt.Errorf("configuration must contain one YAML document")
	}
	if jobdb != "" {
		if len(c.Targets) > 1 {
			return nil, fmt.Errorf("--jobdb cannot override multiple configured targets")
		}
		if len(c.Targets) == 0 {
			c.Targets = []Target{{}}
		}
		c.Targets[0].JobDB = jobdb
	}
	if len(c.Targets) == 0 {
		c.Targets = []Target{{}}
	}
	for i := range c.Targets {
		if c.Targets[i].JobDB == "" {
			uri, err := resolveJobDB(ctx)
			if err != nil {
				return nil, err
			}
			c.Targets[i].JobDB = uri
		}
	}
	if c.Defaults.Image == "" {
		c.Defaults.Image = "ghcr.io/colony-2/base:latest"
	}
	if c.Defaults.Platform == "" {
		c.Defaults.Platform = "linux/" + runtime.GOARCH
		c.AutoPlatform = true
	}
	if c.Defaults.CPU == "" {
		c.Defaults.CPU = "1"
	}
	if c.Defaults.Memory == "" {
		c.Defaults.Memory = "1Gi"
	}
	if c.Defaults.Scratch == "" {
		c.Defaults.Scratch = "1Gi"
	}
	if c.HTTP.Listen == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		c.HTTP.Listen = ":" + port
	}
	_, port, err := net.SplitHostPort(c.HTTP.Listen)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 0 || n > 65535 {
		return nil, fmt.Errorf("http.listen must be a host:port address")
	}
	if c.PollInterval == "" {
		c.PollInterval = "5s"
	}
	if c.Cooldown == "" {
		c.Cooldown = "60s"
	}
	if c.LeaseDuration == "" {
		c.LeaseDuration = "5m"
	}
	var leaseErr error
	c.Lease, leaseErr = time.ParseDuration(c.LeaseDuration)
	if leaseErr != nil || c.Lease < time.Second || c.Lease > 24*time.Hour {
		return nil, fmt.Errorf("lease_duration must be between 1s and 24h")
	}
	if c.CallTimeout == "" {
		c.CallTimeout = "30s"
	}
	if c.ClaimTimeout == "" {
		c.ClaimTimeout = "5s"
	}
	if c.ClaimConcurrency == 0 {
		c.ClaimConcurrency = 8
	}
	if c.ClaimConcurrency < 1 || c.ClaimConcurrency > compute.MaxBatch {
		return nil, fmt.Errorf("claim_concurrency must be between 1 and 100")
	}
	if c.BatchTimeout == "" {
		c.BatchTimeout = "2m"
	}
	for _, v := range []struct {
		s   string
		dst *time.Duration
	}{{c.PollInterval, &c.Poll}, {c.Cooldown, &c.Cool}, {c.CallTimeout, &c.Call}, {c.BatchTimeout, &c.Batch}, {c.ClaimTimeout, &c.Claim}} {
		n, e := time.ParseDuration(v.s)
		if e != nil || n <= 0 || n > 365*24*time.Hour {
			return nil, fmt.Errorf("invalid positive duration %q", v.s)
		}
		*v.dst = n
	}
	if c.Defaults.Timeout != "" {
		timeout, err := time.ParseDuration(c.Defaults.Timeout)
		if err != nil || timeout < time.Second || timeout > 365*24*time.Hour {
			return nil, fmt.Errorf("explicit infrastructure timeout must be between one second and one year")
		}
		c.ExecutionTimeout = timeout
	}
	if c.Defaults.StartWindow != "" {
		n, e := time.ParseDuration(c.Defaults.StartWindow)
		if e != nil || n <= 0 || n > c.Lease {
			return nil, fmt.Errorf("start_window must be positive and no longer than lease_duration")
		}
		c.StartWindow = n
	}
	if c.BatchSize == 0 {
		c.BatchSize = 100
	}
	if c.PerTenant == 0 {
		c.PerTenant = 100
	}
	if c.MaxPages == 0 {
		c.MaxPages = 1000
	}
	if c.BatchSize < 1 || c.BatchSize > 100 || c.PerTenant < 1 || c.MaxPages < 1 {
		return nil, fmt.Errorf("invalid batch/page limits")
	}
	a := compute.Allocation{Image: c.Defaults.Image, Platform: strings.ToLower(c.Defaults.Platform)}
	for _, v := range []struct {
		s   string
		p   *int64
		cpu bool
	}{{c.Defaults.CPU, &a.CPUMillis, true}, {c.Defaults.Memory, &a.MemoryBytes, false}, {c.Defaults.Scratch, &a.ScratchBytes, false}} {
		n, e := quantity.Parse(v.s, v.cpu)
		if e != nil {
			return nil, e
		}
		*v.p = n
	}
	ref, e := reference.ParseNormalizedNamed(a.Image)
	if e != nil {
		return nil, e
	}
	a.Image = reference.TagNameOnly(ref).String()
	if e = a.Validate(); e != nil {
		return nil, e
	}
	c.Allocation = a
	if _, e = c2j.Process("db", "job", a, c.Defaults.Env, nil); e != nil {
		return nil, e
	}
	if len(c.Defaults.Routes) == 0 {
		c.Defaults.Routes = []c2j.Route{{JobType: "recipe"}}
	}
	for _, r := range c.Defaults.Routes {
		if r.JobType == "" {
			return nil, fmt.Errorf("route job_type required")
		}
	}
	if len(c.Targets) == 0 {
		return nil, fmt.Errorf("targets are required")
	}
	if len(c.Providers) == 0 {
		c.Providers = map[string]Provider{"docker": {Type: "docker"}}
		for i := range c.Targets {
			if len(c.Targets[i].Services) == 0 {
				c.Targets[i].Services = []Service{{Name: "docker", Priority: 1}}
			}
		}
	}
	for name, p := range c.Providers {
		if name == "" {
			return nil, fmt.Errorf("empty provider name")
		}
		switch p.Type {
		case "remote", "docker", "cloudrun", "ecs", "azurejobs":
		default:
			return nil, fmt.Errorf("unknown provider type %q", p.Type)
		}
		if p.Type == "docker" {
			if p.DependencyCache == nil {
				p.DependencyCache = &DependencyCache{}
			}
			cache := p.DependencyCache
			if cache.Namespace == "" {
				cache.Namespace = "default"
			}
			if cache.Generation == "" {
				cache.Generation = "1"
			}
			if cache.MaxAge == "" {
				cache.MaxAge = "720h"
			}
			age, err := time.ParseDuration(cache.MaxAge)
			if err != nil || age <= 0 || age > 365*24*time.Hour {
				return nil, fmt.Errorf("invalid dependency_cache.max_age")
			}
			if len(cache.Namespace) > 128 || len(cache.Generation) > 128 || strings.ContainsAny(cache.Namespace+cache.Generation, "\x00\r\n") {
				return nil, fmt.Errorf("invalid dependency_cache namespace or generation")
			}
			if p.Overhead == "" {
				p.Overhead = "256Mi"
			}
			overhead, err := quantity.Parse(p.Overhead, false)
			if err != nil {
				return nil, err
			}
			// Start with a single default-sized job. Larger pools are explicit.
			if p.Capacity.CPU == "" {
				p.Capacity.CPU = quantity.CPU((a.CPUMillis + 9) / 10 * 10)
			}
			if p.Capacity.Memory == "" {
				memory := (a.MemoryBytes+4095)/4096*4096 + (a.ScratchBytes+4095)/4096*4096
				if memory < 6<<20 {
					memory = 6 << 20
				}
				p.Capacity.Memory = quantity.Bytes(memory + overhead)
			}
			if p.Capacity.MaxContainers == 0 {
				p.Capacity.MaxContainers = 1
			}
			c.Providers[name] = p
		} else if p.DependencyCache != nil {
			return nil, fmt.Errorf("dependency_cache is supported only by Docker")
		}
	}
	instances := map[string]string{}
	scopes := map[string]bool{}
	for i, t := range c.Targets {
		u, e := url.Parse(t.JobDB)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("jobdb must be an HTTP(S) tenant URL; embedded c2j databases cannot serve container workers")
		}
		path := strings.Trim(u.Path, "/")
		if path == "" || strings.Contains(path, "/") {
			return nil, fmt.Errorf("jobdb URL must select one tenant")
		}
		c.Targets[i].Tenant = path
		if len(t.Services) == 0 {
			return nil, fmt.Errorf("target needs launch_services when providers are configured")
		}
		endpoint := u.Scheme + "://" + strings.ToLower(u.Host)
		if t.Instance == "" {
			t.Instance = fmt.Sprintf("jobdb-%x", sha256.Sum256([]byte(endpoint)))
			c.Targets[i].Instance = t.Instance
		}
		if prior, ok := instances[t.Instance]; ok && prior != endpoint {
			return nil, fmt.Errorf("instance_id maps to multiple deployments")
		}
		instances[t.Instance] = endpoint
		names := map[string]bool{}
		for _, s := range t.Services {
			if _, ok := c.Providers[s.Name]; !ok || s.Priority < 1 || names[s.Name] {
				return nil, fmt.Errorf("invalid target launch service %q", s.Name)
			}
			names[s.Name] = true
		}
		key := endpoint + "\x00" + path
		if scopes[key] {
			return nil, fmt.Errorf("duplicate tenant target")
		}
		scopes[key] = true
	}
	return c, nil
}
