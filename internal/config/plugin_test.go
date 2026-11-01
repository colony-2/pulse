package config

import (
	"context"
	"encoding/json"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pluginYAML(t *testing.T, block string) string {
	t.Helper()
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	return "defaults:\n  platform: linux/amd64\nproviders:\n  custom:\n    type: plugin\n    plugin:\n      command: " + binary + "\n      args: [CANARY-ARG]\n      config:\n" + block + "\ntargets:\n  - instance_id: test\n    jobdb: https://example.com/tenant\n    launch_services:\n      - name: custom\n        priority: 1\n"
}
func TestPluginConfigurationIsolationAndSources(t *testing.T) {
	data := pluginYAML(t, "        password: CANARY-CONFIG\n        count: 12\n        flag: true\n        string: '12'")
	dir := t.TempDir()
	path := filepath.Join(dir, "pulse.yaml")
	if e := os.WriteFile(path, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	cfg, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.BaseDir != dir {
		t.Fatal(cfg.BaseDir)
	}
	options, e := cfg.Providers["custom"].Plugin.ProcessConfig("custom", cfg.BaseDir)
	if e != nil {
		t.Fatal(e)
	}
	var fields map[string]any
	if e = yaml.Unmarshal(options.ConfigYAML, &fields); e != nil {
		t.Fatal(e)
	}
	if fields["count"] != 12 || fields["flag"] != true || fields["string"] != "12" {
		t.Fatal(fields)
	}
	public, e := cfg.Public()
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(public)
	if strings.Contains(string(b), "CANARY") || strings.Contains(string(b), "base_dir") {
		t.Fatal("public configuration leaked plugin data")
	}
	if !strings.Contains(string(options.ConfigYAML), "CANARY-CONFIG") {
		t.Fatal("public redaction mutated original")
	}
	cfg, e = LoadOptions(context.Background(), path, "")
	if e != nil || cfg.BaseDir != dir {
		t.Fatal(cfg, e)
	}
	t.Setenv("PULSE_CONFIG", data)
	cfg, e = LoadOptions(context.Background(), "", "")
	cwd, _ := os.Getwd()
	if e != nil || cfg.BaseDir != cwd {
		t.Fatal(cfg, e)
	}
}
func TestPluginInvalidConfigurationIsSafe(t *testing.T) {
	for name, block := range map[string]string{"duplicate": "        secret: CANARY\n        secret: duplicate", "alias": "        secret: &secret CANARY\n        copy: *secret", "oversized": "        secret: " + strings.Repeat("x", 1<<20)} {
		t.Run(name, func(t *testing.T) {
			_, e := Parse([]byte(pluginYAML(t, block)))
			if e == nil || strings.Contains(e.Error(), "CANARY") {
				t.Fatal(e)
			}
		})
	}
	valid := pluginYAML(t, "        secret: CANARY")
	for _, s := range []string{
		strings.Replace(valid, "      args: [CANARY-ARG]", "      args: [123]", 1),
		strings.Replace(valid, "      args: [CANARY-ARG]", "      pass_env: [true]", 1),
		strings.Replace(valid, "      args: [CANARY-ARG]", "      max_concurrent: '4'", 1),
		strings.Replace(valid, "      command:", "      unknown: CANARY\n      command:", 1),
		strings.Replace(valid, "    type: plugin", "    type: plugin\n    endpoint: https://example.com", 1),
		strings.Replace(valid, "    type: plugin", "    type: remote", 1),
		strings.Replace(valid, "      args: [CANARY-ARG]", "      max_concurrent: CANARY", 1),
		strings.Replace(valid, "      args: [CANARY-ARG]", "      pass_env: [PLUGIN_CLIENT_CERT]", 1),
		strings.Replace(valid, "      args: [CANARY-ARG]", "      start_timeout: 0s", 1),
	} {
		if _, e := Parse([]byte(s)); e == nil || strings.Contains(e.Error(), "CANARY") {
			t.Fatal(e)
		}
	}
}
