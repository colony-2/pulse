package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestC2JConventionsWithoutPulseConfig(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("PULSE_CONFIG", "")
	os.Unsetenv("PULSE_CONFIG")
	t.Setenv("C2J_JOBDB", "")
	if _, err := LoadSource(""); err == nil || !strings.Contains(err.Error(), "C2J_JOBDB") {
		t.Fatalf("missing tenant should explain c2j settings: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".c2j"), 0700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, ".c2j", "config.yaml")
	if err := os.WriteFile(project, []byte("jobdb: https://jobdb.example/project\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("child", 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(root, "child"))
	cfg, err := LoadSource("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecutionTimeout != 0 || cfg.Defaults.Timeout != "" || cfg.Targets[0].Tenant != "project" || cfg.Targets[0].Instance == "" || cfg.Allocation.Image != "ghcr.io/colony-2/shai-mega:latest" || cfg.Allocation.CPUMillis != 1000 || cfg.Allocation.MemoryBytes != 1<<30 || cfg.Allocation.ScratchBytes != 1<<30 || !cfg.AutoPlatform || cfg.Providers["docker"].Type != "docker" || cfg.Targets[0].Services[0].Name != "docker" {
		t.Fatalf("wrong defaults: %+v", cfg)
	}
	instance := cfg.Targets[0].Instance
	t.Setenv("C2J_JOBDB", "https://jobdb.example/environment")
	cfg, err = LoadSource("")
	if err != nil || cfg.Targets[0].Tenant != "environment" || cfg.Targets[0].Instance != instance {
		t.Fatal(cfg, err)
	}
	cfg, err = LoadOptions(context.Background(), "", "https://other.example/flag")
	if err != nil || cfg.Targets[0].Tenant != "flag" || cfg.Targets[0].Instance == instance {
		t.Fatal(cfg, err)
	}
	// Explicit Pulse targets remain useful for multi-tenant deployments.
	t.Setenv("PULSE_CONFIG", "targets: [{jobdb: https://jobdb.example/explicit}]")
	cfg, err = LoadSource("")
	if err != nil || cfg.Targets[0].Tenant != "explicit" {
		t.Fatal(cfg, err)
	}
	cfg, err = LoadOptions(context.Background(), "", "https://jobdb.example/override")
	if err != nil || cfg.Targets[0].Tenant != "override" {
		t.Fatal(cfg, err)
	}
	// c2j's command-valued settings resolve through its own loader.
	os.Unsetenv("PULSE_CONFIG")
	t.Setenv("C2J_JOBDB", "")
	if err := os.WriteFile(project, []byte("jobdb:\n  command: printf https://jobdb.example/command\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadSource("")
	if err != nil || cfg.Targets[0].Tenant != "command" {
		t.Fatal(cfg, err)
	}
}

func TestTenantTargetsAndDefaultOverrides(t *testing.T) {
	for _, data := range []string{
		"targets: [{jobdb: 'https://db.example/t'}, {jobdb: 'https://db.example/t/'}]",
		"targets: [{jobdb: 'embed:///'}]",
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("invalid targets accepted: %s", data)
		}
	}
	cfg, err := Parse([]byte("targets: [{jobdb: 'https://db.example/t'}]\ndefaults: {image: 'alpine:3', platform: linux/amd64, cpu: '2', memory: 2Gi, scratch: 3Gi}"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoPlatform || cfg.Allocation.Image != "docker.io/library/alpine:3" || cfg.Allocation.CPUMillis != 2000 || cfg.Allocation.MemoryBytes != 2<<30 || cfg.Allocation.ScratchBytes != 3<<30 {
		t.Fatalf("ignored overrides: %+v", cfg.Allocation)
	}
}
