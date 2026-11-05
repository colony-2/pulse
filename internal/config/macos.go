package config

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

const DefaultMacOSBase = "macos-sequoia-15.7.7-arm64"

// This image/version pair is validated by the pinned pulse-tart release.
const DefaultMacOSImage = "ghcr.io/cirruslabs/macos-sequoia-base@sha256:eee659985b01e4df2a6c0754bd8d25e54969933c43fdcd4edef8bea337b3d354"

// MacOSConfig is host-owned configuration for the shared Tart/Orchard library.
// Credentials are resolved from environment variables only during construction.
type MacOSConfig struct {
	OwnerID          string          `yaml:"owner_id"`
	StateDir         string          `yaml:"state_dir"`
	Bases            []MacOSBase     `yaml:"bases"`
	Slots            []MacOSSlot     `yaml:"slots"`
	BootstrapTimeout string          `yaml:"bootstrap_timeout"`
	Tart             *TartOptions    `yaml:"tart"`
	Orchard          *OrchardOptions `yaml:"orchard"`
}
type MacOSBase struct {
	Name           string   `yaml:"name"`
	Image          string   `yaml:"image"`
	OSVersion      string   `yaml:"os_version"`
	SSHUsername    string   `yaml:"ssh_username"`
	SSHPasswordEnv string   `yaml:"ssh_password_env"`
	SSHHostKeyFile string   `yaml:"ssh_host_key_file"`
	Capabilities   []string `yaml:"capabilities"`
}
type MacOSSlot struct {
	Name      string `yaml:"name"`
	Count     int    `yaml:"count"`
	Base      string `yaml:"base"`
	CPU       int64  `yaml:"cpu"`
	MemoryMiB int64  `yaml:"memory_mib"`
}
type TartOptions struct {
	Binary         string `yaml:"binary"`
	Home           string `yaml:"home"`
	CommandTimeout string `yaml:"command_timeout"`
	SSHTimeout     string `yaml:"ssh_timeout"`
}
type OrchardOptions struct {
	URL            string            `yaml:"url"`
	Worker         string            `yaml:"worker"`
	WorkerLabels   map[string]string `yaml:"worker_labels"`
	Username       string            `yaml:"username"`
	TokenEnv       string            `yaml:"token_env"`
	CACertFile     string            `yaml:"ca_cert_file"`
	AllowHTTP      bool              `yaml:"allow_http"`
	RequestTimeout string            `yaml:"request_timeout"`
}

func MacOSOptions(p Provider) (MacOSConfig, error) {
	var c MacOSConfig
	raw, err := yaml.Marshal(p.Options)
	if err != nil {
		return c, fmt.Errorf("invalid macOS provider config")
	}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if d.Decode(&c) != nil {
		return c, fmt.Errorf("invalid macOS provider config")
	}
	if p.Type == "tart" && c.Orchard != nil || p.Type == "orchard" && (c.Tart != nil || c.Orchard == nil) {
		return c, fmt.Errorf("provider requires matching tart/orchard connection settings")
	}
	if c.Bases == nil {
		c.Bases = []MacOSBase{{Name: DefaultMacOSBase, Image: DefaultMacOSImage, OSVersion: "15.7.7", SSHUsername: "admin", Capabilities: []string{"nix", "git", "node", "pnpm", "uv", "python"}}}
	}
	if c.Slots == nil {
		c.Slots = []MacOSSlot{{Name: "standard", Count: 4, Base: DefaultMacOSBase}}
	}
	return c, nil
}

// Automatic providers only apply when the provider map is omitted or empty.
// Detection is observational: never run Tart or prepare VMs while loading YAML.
func automaticProviders(goos, goarch string, lookPath func(string) (string, error)) (map[string]Provider, []Service, string) {
	providers := map[string]Provider{"docker": {Type: "docker"}}
	services := []Service{{Name: "docker", Priority: 1}}
	if goos == "darwin" && goarch == "arm64" {
		if _, err := lookPath("tart"); err == nil {
			providers["tart"] = Provider{Type: "tart"}
			services = append(services, Service{Name: "tart", Priority: 2})
			return providers, services, "enabled: tart found on PATH"
		}
		return providers, services, "disabled: tart not found on PATH"
	}
	return providers, services, fmt.Sprintf("disabled: requires darwin/arm64; Pulse is running as %s/%s", goos, goarch)
}
