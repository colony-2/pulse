package config

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultProviderOverride uses nil fields for omitted CLI options.
type DefaultProviderOverride struct {
	Enabled    *bool
	ConfigYAML *string
}

func (c *Config) configureProviders(overrides Overrides) error {
	if len(c.Providers) != 0 {
		if overrides.Docker != (DefaultProviderOverride{}) || overrides.VZ != (DefaultProviderOverride{}) {
			return fmt.Errorf("default provider flags require an omitted or empty providers map")
		}
		c.VZAutoDiscovery = "disabled: explicit providers configured"
		return nil
	}
	if c.DynamicProvider != nil {
		if overrides.Docker != (DefaultProviderOverride{}) || overrides.VZ != (DefaultProviderOverride{}) {
			return fmt.Errorf("default provider flags cannot be used with dynamic_provider")
		}
		c.VZAutoDiscovery = "disabled: dynamic providers configured"
		return nil
	}
	providers, services, reason, err := automaticProviders(supportsVZ(), overrides)
	if err != nil {
		return err
	}
	c.Providers, c.VZAutoDiscovery = providers, reason
	for i := range c.Targets {
		if len(c.Targets[i].Services) == 0 {
			c.Targets[i].Services = slices.Clone(services)
		}
	}
	return nil
}

func automaticProviders(vzSupported bool, overrides Overrides) (map[string]Provider, []Service, string, error) {
	providers := make(map[string]Provider, 2)
	var services []Service
	for _, entry := range []struct {
		service   Service
		supported bool
		override  DefaultProviderOverride
	}{
		{Service{Name: "docker", Priority: 1}, true, overrides.Docker},
		{Service{Name: "vz", Priority: 2}, vzSupported, overrides.VZ},
	} {
		name := entry.service.Name
		p, err := entry.override.provider(name, entry.supported)
		if err != nil {
			return nil, nil, "", err
		}
		if p.Type != "" {
			providers[name] = p
			services = append(services, entry.service)
		}
	}
	if len(providers) == 0 {
		return nil, nil, "", fmt.Errorf("no default providers enabled")
	}
	switch {
	case overrides.VZ.Enabled != nil && !*overrides.VZ.Enabled:
		return providers, services, "disabled: --vz=false", nil
	case overrides.VZ.Enabled != nil:
		return providers, services, "enabled: --vz", nil
	case vzSupported:
		return providers, services, "enabled: Apple Silicon with macOS 14 or later", nil
	default:
		return providers, services, "disabled: requires macOS 14 or later on Apple Silicon", nil
	}
}

func (o DefaultProviderOverride) provider(name string, supported bool) (Provider, error) {
	flag := "--" + name + "-config"
	if o.Enabled != nil && !*o.Enabled {
		if o.ConfigYAML != nil {
			return Provider{}, fmt.Errorf("%s cannot be used with --%s=false", flag, name)
		}
		return Provider{}, nil
	}
	if !supported {
		if o.Enabled != nil || o.ConfigYAML != nil {
			return Provider{}, fmt.Errorf("default VZ provider requires macOS 14 or later on Apple Silicon")
		}
		return Provider{}, nil
	}
	p := Provider{Type: name}
	if o.ConfigYAML == nil {
		return p, nil
	}
	var settings *Provider
	dst := any(&settings)
	if name == "vz" {
		dst = &p.Options
	}
	d := yaml.NewDecoder(strings.NewReader(*o.ConfigYAML))
	d.KnownFields(true)
	var extra any
	if d.Decode(dst) != nil || d.Decode(&extra) != io.EOF || (settings == nil && p.Options == nil) {
		return Provider{}, fmt.Errorf("%s requires one YAML mapping with valid provider settings", flag)
	}
	if settings != nil {
		if settings.Type != "" && settings.Type != name {
			return Provider{}, fmt.Errorf("%s cannot change the provider type", flag)
		}
		p = *settings
		p.Type = name
	}
	return p, nil
}
