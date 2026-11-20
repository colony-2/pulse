package config

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultProviderOverrides(t *testing.T) {
	on, off := true, false
	dockerYAML := "capacity: {max_containers: 2}\ndependency_cache: {enabled: false}"
	vzYAML := "vm_counts: {macos: 1, linux: 0}\nvz: {home: vm-home}"
	providers, services, reason, err := automaticProviders(true, Overrides{
		Docker: DefaultProviderOverride{Enabled: &off},
		VZ:     DefaultProviderOverride{Enabled: &on, ConfigYAML: &vzYAML},
	})
	if err != nil || len(providers) != 1 || providers["vz"].Type != "vz" ||
		!slices.Equal(services, []Service{{Name: "vz", Priority: 2}}) || reason != "enabled: --vz" {
		t.Fatal(providers, services, reason, err)
	}
	vz, err := MacOSOptions(providers["vz"])
	if err != nil || vz.VZ == nil || vz.VZ.Home != "vm-home" || vz.VMCounts.MacOS == nil || *vz.VMCounts.MacOS != 1 ||
		vz.VMCounts.Linux == nil || *vz.VMCounts.Linux != 0 {
		t.Fatal(vz, err)
	}

	for _, supported := range []bool{true, false} {
		providers, services, reason, err = automaticProviders(supported, Overrides{
			Docker: DefaultProviderOverride{ConfigYAML: &dockerYAML},
			VZ:     DefaultProviderOverride{Enabled: &off},
		})
		docker := providers["docker"]
		if err != nil || len(providers) != 1 || docker.Type != "docker" || docker.Capacity.MaxContainers != 2 ||
			docker.DependencyCache == nil || docker.DependencyCache.Enabled == nil || *docker.DependencyCache.Enabled ||
			!slices.Equal(services, []Service{{Name: "docker", Priority: 1}}) || reason != "disabled: --vz=false" {
			t.Fatal(providers, services, reason, err)
		}
	}
	for _, o := range []DefaultProviderOverride{{Enabled: &on}, {ConfigYAML: &vzYAML}} {
		if _, _, _, err := automaticProviders(false, Overrides{VZ: o}); err == nil || !strings.Contains(err.Error(), "requires macOS 14") {
			t.Fatal("explicit VZ request on unsupported host", err)
		}
	}
}

func TestDefaultProviderFlagsWithConfiguration(t *testing.T) {
	off := false
	settings := "capacity: {max_containers: 2}"
	overrides := Overrides{JobDB: "https://db.example/tenant", Docker: DefaultProviderOverride{ConfigYAML: &settings}, VZ: DefaultProviderOverride{Enabled: &off}}
	cfg, err := parse(t.Context(), []byte("providers: {}"), overrides)
	if err != nil || cfg.Providers["docker"].Capacity.MaxContainers != 2 || cfg.Providers["docker"].Overhead != "256Mi" ||
		!slices.Equal(cfg.Targets[0].Services, []Service{{Name: "docker", Priority: 1}}) {
		t.Fatal(cfg, err)
	}
	for _, tc := range []struct{ config, want string }{
		{"providers: {custom: {type: docker}}", "default provider flags require an omitted or empty providers map"},
		{"targets: [{launch_services: [{name: vz, priority: 1}]}]", `invalid target launch service "vz"`},
	} {
		if _, err := parse(t.Context(), []byte(tc.config), overrides); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %q, got %v", tc.want, err)
		}
	}
	for _, settings := range []string{"null", "[]", "unknown: true", "{}\n---\n{}", "type: remote"} {
		overrides.Docker.ConfigYAML = &settings
		if _, err := parse(t.Context(), []byte("{}"), overrides); err == nil || !strings.Contains(err.Error(), "--docker-config") {
			t.Fatalf("expected flag error for %q, got %v", settings, err)
		}
	}
}
