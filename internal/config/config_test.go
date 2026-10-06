package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `defaults:
  image: alpine:3
  platform: linux/arm64
  cpu: "1"
  memory: 1Gi
  scratch: 1Gi
providers:
  pool:
    type: remote
    endpoint: https://pool.example
    token_env: POOL_TOKEN
targets:
  - instance_id: test
    jobdb: https://db.example/acme
    cells: [github.com/acme/app]
    launch_services: [{name: pool, priority: 1}]
`

func TestConfig(t *testing.T) {
	c, e := Parse([]byte(valid))
	if e != nil || c.Targets[0].Tenant != "acme" || c.Allocation.MemoryBytes != 1<<30 {
		t.Fatal(c, e)
	}
	if _, e = Parse([]byte(valid + "unknown_option: true\n")); e == nil {
		t.Fatal("unknown field ignored")
	}
	if _, e = Parse([]byte(valid + "---\n{}")); e == nil {
		t.Fatal("multiple documents ignored")
	}
}

func TestExampleConfigurations(t *testing.T) {
	paths, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatal("missing example configurations", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := Load(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemovedListingOptions(t *testing.T) {
	for _, addition := range []string{"c2j:\n  mode: external\n", "c2j:\n  mode: embedded\n", "c2j:\n  executable: c2j\n", "c2j:\n  env: {C2J_JOBDB: db}\n"} {
		if _, err := Parse([]byte(valid + addition)); err == nil {
			t.Fatal("removed listing settings accepted", addition)
		}
	}
	for _, cell := range []string{"./repo", "/srv/repo", "my-alias"} {
		if _, err := Parse([]byte(strings.Replace(valid, "github.com/acme/app", cell, 1))); err == nil {
			t.Fatal("repository discovery accepted", cell)
		}
	}
}

func TestHTTPListenConfiguration(t *testing.T) {
	for _, tt := range []struct{ port, listen, want string }{
		{"", "", ":8080"}, {"9000", "", ":9000"},
		{"invalid", "127.0.0.1:9001", "127.0.0.1:9001"},
		{"", "[::1]:8080", "[::1]:8080"},
		{"", "127.0.0.1:0", "127.0.0.1:0"},
		{"invalid", "", ""}, {"", "localhost", ""}, {"", ":65536", ""},
	} {
		t.Run(tt.port+"/"+tt.listen, func(t *testing.T) {
			t.Setenv("PORT", tt.port)
			data := valid
			if tt.listen != "" {
				data += "http:\n  listen: '" + tt.listen + "'\n"
			}
			cfg, err := Parse([]byte(data))
			if tt.want == "" {
				if err == nil {
					t.Fatal("invalid address accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HTTP.Listen != tt.want {
				t.Fatalf("got %q want %q", cfg.HTTP.Listen, tt.want)
			}
		})
	}
}

func TestConfigurationSourceSelection(t *testing.T) {
	t.Chdir(t.TempDir())
	file := []byte(valid)
	if err := os.WriteFile("pulse.yaml", file, 0600); err != nil {
		t.Fatal(err)
	}
	inline := strings.Replace(valid, "alpine:3", "alpine:env", 1)
	for _, tt := range []struct {
		name, path, value, wantImage, wantError string
		set                                     bool
	}{
		{name: "default file", wantImage: "alpine:3"},
		{name: "inline over default", set: true, value: inline, wantImage: "alpine:env"},
		{name: "explicit file over inline", path: "pulse.yaml", set: true, value: inline, wantImage: "alpine:3"},
		{name: "explicit file over invalid inline", path: "pulse.yaml", set: true, value: "[invalid", wantImage: "alpine:3"},
		{name: "missing explicit file", path: "missing.yaml", set: true, value: inline, wantError: "missing.yaml"},
		{name: "empty inline", set: true, wantError: "PULSE_CONFIG"},
		{name: "blank inline", set: true, value: " \n\t", wantError: "PULSE_CONFIG"},
		{name: "malformed inline", set: true, value: "[invalid", wantError: "PULSE_CONFIG"},
		{name: "invalid inline", set: true, value: "{}", wantError: "PULSE_CONFIG"},
		{name: "unknown inline field", set: true, value: inline + "unknown_option: true\n", wantError: "PULSE_CONFIG"},
		{name: "multiple documents", set: true, value: inline + "---\n{}", wantError: "PULSE_CONFIG"},
		{name: "inline is contents not path", set: true, value: "pulse.yaml", wantError: "PULSE_CONFIG"},
		{name: "inline is not merged with file", set: true, value: "poll_interval: 1s", wantError: "PULSE_CONFIG"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PULSE_CONFIG", tt.value)
			if !tt.set {
				if err := os.Unsetenv("PULSE_CONFIG"); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := LoadSource(tt.path)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected %q error, got %v", tt.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Defaults.Image != tt.wantImage {
				t.Fatalf("got %q want %q", cfg.Defaults.Image, tt.wantImage)
			}
		})
	}
	// The inline document is literal; no shell expansion is performed.
	t.Setenv("PULSE_CONFIG", strings.Replace(inline, "defaults:\n", "defaults:\n  env: {LITERAL: '${SHOULD_NOT_EXPAND}'}\n", 1))
	t.Setenv("SHOULD_NOT_EXPAND", "expanded")
	cfg, err := LoadSource("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Env["LITERAL"] != "${SHOULD_NOT_EXPAND}" {
		t.Fatal("configuration was expanded")
	}
}

func TestLeaseDurationAndStartWindow(t *testing.T) {
	for _, tc := range []struct {
		lease, window string
		valid         bool
	}{
		{"", "", true}, {"1s", "1s", true}, {"24h", "2m", true},
		{"5m", "2m", true}, {"5m", "6m", false}, {"0s", "", false},
		{"500ms", "", false}, {"25h", "", false}, {"invalid", "", false},
	} {
		t.Run(tc.lease+"/"+tc.window, func(t *testing.T) {
			data := valid
			if tc.lease != "" {
				data += "lease_duration: " + tc.lease + "\n"
			}
			if tc.window != "" {
				data = strings.Replace(data, "defaults:\n", "defaults:\n  start_window: "+tc.window+"\n", 1)
			}
			cfg, err := Parse([]byte(data))
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
			if err == nil && tc.lease == "" && cfg.Lease != 5*time.Minute {
				t.Fatal("incorrect lease default")
			}
		})
	}
}

func TestClaimBatchLimits(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaimConcurrency != 8 || cfg.Claim != 5*time.Second || cfg.BatchSize != 100 {
		t.Fatal("wrong claim defaults")
	}
	cfg, err = Parse([]byte(valid + "claim_concurrency: 3\nclaim_timeout: 250ms\nbatch_size: 12\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaimConcurrency != 3 || cfg.Claim != 250*time.Millisecond || cfg.BatchSize != 12 {
		t.Fatal("claim settings ignored")
	}
	for _, extra := range []string{"claim_concurrency: -1\n", "claim_concurrency: 101\n", "claim_timeout: 0s\n", "claim_timeout: -1s\n", "claim_timeout: invalid\n", "batch_size: 101\n"} {
		if _, err := Parse([]byte(valid + extra)); err == nil {
			t.Fatal("invalid limit accepted", extra)
		}
	}
}

func TestDefaultDockerProvider(t *testing.T) {
	start := strings.Index(valid, "providers:\n")
	end := strings.Index(valid, "targets:\n")
	base := valid[:start] + valid[end:]
	base = strings.Replace(base, "    launch_services: [{name: pool, priority: 1}]\n", "", 1)
	for _, providers := range []string{"", "providers: {}\n", "providers: null\n"} {
		t.Run(providers, func(t *testing.T) {
			cfg, err := Parse([]byte(base + providers))
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Providers["docker"]
			if len(cfg.Providers) != 1 || p.Type != "docker" || p.Socket != "unix:///var/run/docker.sock" || p.Capacity.CPU != "1000m" || p.Capacity.Memory != "2359296Ki" || p.Capacity.MaxContainers != 1 || p.Overhead != "256Mi" {
				t.Fatalf("unexpected Docker defaults: %+v", p)
			}
			if services := cfg.Targets[0].Services; len(services) != 1 || services[0] != (Service{Name: "docker", Priority: 1}) {
				t.Fatalf("unexpected launch services: %+v", services)
			}
		})
	}
	if _, err := Parse([]byte(base + "    launch_services: [{name: typo, priority: 1}]\n")); err == nil {
		t.Fatal("unknown explicit service silently replaced")
	}
	cfg, err := Parse([]byte(base + "    launch_services: [{name: docker, priority: 2}]\n"))
	if err != nil || cfg.Targets[0].Services[0].Priority != 2 {
		t.Fatal("explicit Docker route not preserved", err)
	}
}

func TestExplicitProviderDoesNotAddDocker(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil || len(cfg.Providers) != 1 || cfg.Providers["pool"].Type != "remote" {
		t.Fatal(cfg, err)
	}
	for _, data := range []string{
		strings.Replace(valid, "type: remote", "type: invalid", 1),
		strings.Replace(valid, "    launch_services: [{name: pool, priority: 1}]\n", "", 1),
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatal("invalid explicit provider configuration fell back to Docker")
		}
	}
}

func TestDockerBudgetDefaultsRoundUp(t *testing.T) {
	data := strings.Replace(valid, "type: remote", "type: docker", 1)
	data = strings.Replace(data, `cpu: "1"`, `cpu: "1001m"`, 1)
	data = strings.ReplaceAll(data, "1Gi", "1Ki")
	cfg, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["pool"]
	if p.Capacity.CPU != "1010m" || p.Capacity.Memory != "268288Ki" {
		t.Fatalf("budget cannot fit rounded default job: %+v", p)
	}
	cfg, err = Load("../../examples/docker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p = cfg.Providers["local"]
	if p.Capacity.CPU != "4" || p.Capacity.Memory != "8Gi" || p.Capacity.MaxContainers != 4 || p.Helper != "/usr/local/lib/pulse/pulse-exec" {
		t.Fatalf("explicit Docker configuration changed: %+v", p)
	}
}
