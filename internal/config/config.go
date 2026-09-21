package config

import (
	"bytes"
	"fmt"
	"github.com/colony-2/cortex/internal/c2j"
	"github.com/colony-2/cortex/internal/quantity"
	"github.com/colony-2/cortex/pkg/compute"
	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

type Service struct {
	Name     string `yaml:"name"`
	Priority int    `yaml:"priority"`
}
type Target struct {
	Instance string    `yaml:"instance_id"`
	JobDB    string    `yaml:"jobdb"`
	Cells    []string  `yaml:"cells"`
	Services []Service `yaml:"launch_services"`
	Tenant   string    `yaml:"-"`
}
type Provider struct {
	SupervisorPath     string            `yaml:"supervisor_path"`
	ImageStorageBounds map[string]string `yaml:"image_storage_bounds"`
	MaxAzureCPU        int64             `yaml:"max_azure_cpu_millis"`

	Type      string `yaml:"type"`
	Endpoint  string `yaml:"endpoint"`
	TokenEnv  string `yaml:"token_env"`
	AllowHTTP bool   `yaml:"allow_http"`
	Socket    string `yaml:"socket"`
	Helper    string `yaml:"helper"`
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
type Config struct {
	PollInterval string `yaml:"poll_interval"`
	Cooldown     string `yaml:"cooldown"`
	CallTimeout  string `yaml:"call_timeout"`
	BatchTimeout string `yaml:"batch_timeout"`
	BatchSize    int    `yaml:"batch_size"`
	PerCell      int    `yaml:"max_jobs_per_cell"`
	MaxPages     int    `yaml:"max_pages_per_cell"`
	C2J          struct {
		Executable      string            `yaml:"executable"`
		ExpectedVersion string            `yaml:"expected_version"`
		WorkingDir      string            `yaml:"working_dir"`
		Env             map[string]string `yaml:"env"`
	} `yaml:"c2j"`
	Defaults struct {
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
	Poll, Cool, Call, Batch, ExecutionTimeout, StartWindow time.Duration       `yaml:"-"`
}

func Load(path string) (*Config, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	return Parse(b)
}
func Parse(b []byte) (*Config, error) {
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
	if c.PollInterval == "" {
		c.PollInterval = "5s"
	}
	if c.Cooldown == "" {
		c.Cooldown = "60s"
	}
	if c.CallTimeout == "" {
		c.CallTimeout = "30s"
	}
	if c.BatchTimeout == "" {
		c.BatchTimeout = "2m"
	}
	if c.Defaults.Timeout == "" {
		c.Defaults.Timeout = "1h"
	}
	for _, v := range []struct {
		s   string
		dst *time.Duration
	}{{c.PollInterval, &c.Poll}, {c.Cooldown, &c.Cool}, {c.CallTimeout, &c.Call}, {c.BatchTimeout, &c.Batch}, {c.Defaults.Timeout, &c.ExecutionTimeout}} {
		n, e := time.ParseDuration(v.s)
		if e != nil || n <= 0 || n > 365*24*time.Hour {
			return nil, fmt.Errorf("invalid positive duration %q", v.s)
		}
		*v.dst = n
	}
	if c.ExecutionTimeout < time.Second {
		return nil, fmt.Errorf("execution timeout must be at least one second")
	}
	if c.Defaults.StartWindow != "" {
		n, e := time.ParseDuration(c.Defaults.StartWindow)
		if e != nil || n <= 0 || n > c.Cool {
			return nil, fmt.Errorf("start_window must be positive and no longer than cooldown")
		}
		c.StartWindow = n
	}
	if c.BatchSize == 0 {
		c.BatchSize = 100
	}
	if c.PerCell == 0 {
		c.PerCell = 100
	}
	if c.MaxPages == 0 {
		c.MaxPages = 1000
	}
	if c.BatchSize < 1 || c.BatchSize > 100 || c.PerCell < 1 || c.MaxPages < 1 {
		return nil, fmt.Errorf("invalid batch/page limits")
	}
	if c.C2J.Executable == "" {
		c.C2J.Executable = "c2j"
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
	if _, e = c2j.Process("db", "job", "launch", a, c.Defaults.Env, nil); e != nil {
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
	if len(c.Targets) == 0 || len(c.Providers) == 0 {
		return nil, fmt.Errorf("targets and providers are required")
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
	}
	instances := map[string]string{}
	scopes := map[string]bool{}
	for i, t := range c.Targets {
		u, e := url.Parse(t.JobDB)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("invalid jobdb URL")
		}
		path := strings.Trim(u.Path, "/")
		if path == "" || strings.Contains(path, "/") {
			return nil, fmt.Errorf("jobdb URL must select one tenant")
		}
		c.Targets[i].Tenant = path
		if t.Instance == "" || len(t.Cells) == 0 || len(t.Services) == 0 {
			return nil, fmt.Errorf("target needs instance_id, cells and launch_services")
		}
		endpoint := u.Scheme + "://" + u.Host
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
		for _, cell := range t.Cells {
			if strings.TrimSpace(cell) == "" {
				return nil, fmt.Errorf("empty cell")
			}
			key := t.Instance + "\x00" + path + "\x00" + cell
			if scopes[key] {
				return nil, fmt.Errorf("duplicate target/cell scope")
			}
			scopes[key] = true
		}
	}
	return c, nil
}
