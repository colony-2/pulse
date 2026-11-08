package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAutomaticTartDetection(t *testing.T) {
	for _, tc := range []struct {
		os, arch        string
		available, want bool
	}{
		{"darwin", "arm64", true, true}, {"darwin", "arm64", false, false},
		{"darwin", "amd64", true, false}, {"linux", "arm64", true, false}, {"linux", "amd64", true, false},
	} {
		t.Run(tc.os+tc.arch+map[bool]string{true: "-installed", false: "-missing"}[tc.available], func(t *testing.T) {
			called := false
			providers, services, reason := automaticProviders(tc.os, tc.arch, func(name string) (string, error) {
				called = true
				if name != "tart" {
					t.Fatal(name)
				}
				if tc.available {
					return "/opt/homebrew/bin/tart", nil
				}
				return "", errors.New("not installed")
			})
			_, got := providers["tart"]
			wantReason := "requires darwin/arm64"
			if tc.want {
				wantReason = "enabled: tart found on PATH"
			} else if tc.os == "darwin" && tc.arch == "arm64" {
				wantReason = "tart not found on PATH"
			}
			if !strings.Contains(reason, wantReason) {
				t.Fatal("incorrect discovery explanation", reason)
			}
			if got != tc.want || providers["docker"].Type != "docker" || called != (tc.os == "darwin" && tc.arch == "arm64") {
				t.Fatal(providers, called)
			}
			want := []Service{{Name: "docker", Priority: 1}}
			if tc.want {
				want = append(want, Service{Name: "tart", Priority: 2})
				c, err := MacOSOptions(providers["tart"])
				if err != nil || c.Slots != nil || c.Bases != nil {
					t.Fatal(c, err)
				}
			}
			if !reflect.DeepEqual(services, want) {
				t.Fatal(services)
			}
		})
	}
}
func TestMacOSConfiguration(t *testing.T) {
	for _, tc := range []struct {
		kind, options string
		valid         bool
	}{
		{"tart", "{}", true},
		{"tart", "{slots: [{name: standard, count: 0, base: macos-custom}]}", true},
		{"orchard", "{orchard: {url: https://orchard.example, worker: mac, token_env: ORCHARD_TOKEN, username: pulse}}", true},
		{"tart", "{bases: [{name: macos-custom, aliases: [darwin]}]}", false},
		{"tart", "{slotz: 4}", false}, {"tart", "{tart: {unknown: true}}", false},
		{"tart", "{orchard: {url: https://orchard.example}}", false},
		{"orchard", "{}", false}, {"orchard", "{tart: {}}", false},
	} {
		cfg, err := Parse([]byte("providers:\n  mac:\n    type: " + tc.kind + "\n    config: " + tc.options + "\ntargets: [{jobdb: https://db/tenant, launch_services: [{name: mac, priority: 1}]}]"))
		if (err == nil) != tc.valid {
			t.Fatalf("%s %s: %v", tc.kind, tc.options, err)
		}
		if err == nil && (len(cfg.Providers) != 1 || cfg.Providers["mac"].Type != tc.kind) {
			t.Fatal("explicit providers altered")
		}
		if err == nil && cfg.TartAutoDiscovery != "disabled: explicit providers configured" {
			t.Fatal("explicit provider selection not explained", cfg.TartAutoDiscovery)
		}
	}
	c, err := MacOSOptions(Provider{Type: "tart", Options: map[string]any{"slots": []any{map[string]any{"name": "standard", "count": 0, "base": "macos-custom"}}}})
	if err != nil || c.Slots[0].Count != 0 {
		t.Fatal("draining group replaced by default", err)
	}
}
