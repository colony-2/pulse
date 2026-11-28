package config

import (
	"strings"
	"testing"
)

func TestDynamicProviderConfiguration(t *testing.T) {
	const listener = `
dynamic_provider:
  listen: "127.0.0.1:8443"
  tls_cert: collector.pem
  tls_key: collector.key
  auth: {client_ca: workers-ca.pem}
  state_file: registry.json
`
	const target = `
targets:
  - jobdb: https://jobdb.example.com/tenant
    dynamic_provider_priority: 2
`
	cfg, err := Parse([]byte(listener + target))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 0 || cfg.Targets[0].DynamicProviderPriority != 2 {
		t.Fatalf("dynamic-only configuration selected static providers or lost its priority: %#v", cfg)
	}
	for _, test := range []struct {
		name, data, message string
	}{
		{"no opt-in", target, "dynamic_provider_priority"},
		{"no auth", strings.Replace(listener, "  auth: {client_ca: workers-ca.pem}\n", "", 1) + target, "auth.client_ca"},
		{"no state", strings.Replace(listener, "  state_file: registry.json\n", "", 1) + target, "state_file"},
		{"negative priority", listener + strings.Replace(target, "priority: 2", "priority: -1", 1), "dynamic_provider_priority"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse([]byte(test.data)); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected %q error, got %v", test.message, err)
			}
		})
	}
}
