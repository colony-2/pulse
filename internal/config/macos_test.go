package config

import (
	"reflect"
	"testing"
)

func TestAutomaticVZSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supported bool
		reason    string
	}{
		{"supported", true, "enabled: Apple Silicon with macOS 14 or later"},
		{"unsupported", false, "disabled: requires macOS 14 or later on Apple Silicon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providers, services, reason := automaticProviders(tc.supported)
			wantProviders := map[string]Provider{"docker": {Type: "docker"}}
			wantServices := []Service{{Name: "docker", Priority: 1}}
			if tc.supported {
				wantProviders["vz"] = Provider{Type: "vz"}
				wantServices = append(wantServices, Service{Name: "vz", Priority: 2})
			}
			if !reflect.DeepEqual(providers, wantProviders) || !reflect.DeepEqual(services, wantServices) || reason != tc.reason {
				t.Fatal(providers, services, reason)
			}
		})
	}
}

func TestMacOSConfiguration(t *testing.T) {
	for _, tc := range []struct {
		kind, options string
		valid         bool
	}{
		{"vz", "{}", true},
		{"vz", "{slots: [{name: standard, count: 0, base: macos-custom}]}", true},
		{"vz", "{vz: {runner: bin/runner, home: vm-home, delivery_timeout: 2m}}", true},
		{"vz", "{bases: [{name: macos-custom, aliases: [darwin]}]}", false},
		{"vz", "{slotz: 4}", false},
		{"vz", "{vz: {unknown: true}}", false},
		{"vz", "{vz: {binary: bin/runner}}", false},
		{"tart", "{}", false},
		{"orchard", "{}", false},
	} {
		cfg, err := Parse([]byte("providers:\n  mac:\n    type: " + tc.kind + "\n    config: " + tc.options + "\ntargets: [{jobdb: https://db/tenant, launch_services: [{name: mac, priority: 1}]}]"))
		if (err == nil) != tc.valid {
			t.Fatalf("%s %s: %v", tc.kind, tc.options, err)
		}
		if err == nil && (len(cfg.Providers) != 1 || cfg.Providers["mac"].Type != tc.kind) {
			t.Fatal("explicit providers altered")
		}
		if err == nil && cfg.VZAutoDiscovery != "disabled: explicit providers configured" {
			t.Fatal("explicit provider selection not explained", cfg.VZAutoDiscovery)
		}
	}
	c, err := MacOSOptions(Provider{Type: "vz", Options: map[string]any{"slots": []any{map[string]any{"name": "standard", "count": 0, "base": "macos-custom"}}}})
	if err != nil || c.Slots[0].Count != 0 {
		t.Fatal("draining group replaced by default", err)
	}
}
