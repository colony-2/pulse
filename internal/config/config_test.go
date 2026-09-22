package config

import (
	"path/filepath"
	"testing"
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

func TestListingModes(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil || c.C2J.Mode != "embedded" || c.C2J.Executable != "" {
		t.Fatal(c, err)
	}
	for _, addition := range []string{"c2j:\n  mode: invalid\n", "c2j:\n  executable: c2j\n", "c2j:\n  env: {C2J_JOBDB: db}\n"} {
		if _, err := Parse([]byte(valid + addition)); err == nil {
			t.Fatal("invalid mode configuration accepted", addition)
		}
	}
	c, err = Parse([]byte(valid + "c2j:\n  mode: external\n"))
	if err != nil || c.C2J.Executable != "c2j" {
		t.Fatal(c, err)
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
