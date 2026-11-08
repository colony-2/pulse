package config

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// MacOSConfig is host-owned configuration for the shared Tart/Orchard library.
// Credentials are resolved from environment variables only during construction.
type MacOSConfig struct {
	OwnerID          string          `yaml:"owner_id"`
	StateDir         string          `yaml:"state_dir"`
	Bases            []MacOSBase     `yaml:"bases"`
	Slots            []MacOSSlot     `yaml:"slots"`
	VMCounts         MacOSVMCounts   `yaml:"vm_counts"`
	BootstrapTimeout string          `yaml:"bootstrap_timeout"`
	Docker           *MacOSDocker    `yaml:"docker"`
	Tart             *TartOptions    `yaml:"tart"`
	Orchard          *OrchardOptions `yaml:"orchard"`
}

// Nil uses the library convention; zero explicitly disables or drains capacity.
type MacOSVMCounts struct {
	MacOS *int `yaml:"macos"`
	Linux *int `yaml:"linux"`
	Total *int `yaml:"total"`
}
type MacOSBase struct {
	Name           string   `yaml:"name"`
	OS             string   `yaml:"os"`
	Image          string   `yaml:"image"`
	OSVersion      string   `yaml:"os_version"`
	SSHUsername    string   `yaml:"ssh_username"`
	SSHPasswordEnv string   `yaml:"ssh_password_env"`
	SSHHostKeyFile string   `yaml:"ssh_host_key_file"`
	Capabilities   []string `yaml:"capabilities"`
}
type MacOSSlot struct {
	Name          string `yaml:"name"`
	Count         int    `yaml:"count"`
	Base          string `yaml:"base"`
	CPU           int64  `yaml:"cpu"`
	MemoryMiB     int64  `yaml:"memory_mib"`
	DockerProfile string `yaml:"docker_profile"`
}
type MacOSDocker struct {
	Profiles     []MacOSDockerProfile `yaml:"profiles"`
	ReadyTimeout string               `yaml:"ready_timeout"`
}
type MacOSDockerProfile struct {
	Name           string `yaml:"name"`
	Image          string `yaml:"image"`
	SSHUsername    string `yaml:"ssh_username"`
	SSHPasswordEnv string `yaml:"ssh_password_env"`
	SSHHostKeyFile string `yaml:"ssh_host_key_file"`
	CPU            int64  `yaml:"cpu"`
	MemoryMiB      int64  `yaml:"memory_mib"`
	DiskGiB        int64  `yaml:"disk_gib"`
	EngineVersion  string `yaml:"engine_version"`
}
type MacOSDockerEndpoint struct {
	Address string `yaml:"address"`
	PortMin int    `yaml:"port_min"`
	PortMax int    `yaml:"port_max"`
}
type TartOptions struct {
	Binary         string              `yaml:"binary"`
	Home           string              `yaml:"home"`
	CommandTimeout string              `yaml:"command_timeout"`
	SSHTimeout     string              `yaml:"ssh_timeout"`
	DockerEndpoint MacOSDockerEndpoint `yaml:"docker_endpoint"`
	PythonBinary   string              `yaml:"python_binary"`
}
type OrchardOptions struct {
	URL            string              `yaml:"url"`
	Worker         string              `yaml:"worker"`
	WorkerLabels   map[string]string   `yaml:"worker_labels"`
	Username       string              `yaml:"username"`
	TokenEnv       string              `yaml:"token_env"`
	CACertFile     string              `yaml:"ca_cert_file"`
	AllowHTTP      bool                `yaml:"allow_http"`
	RequestTimeout string              `yaml:"request_timeout"`
	DockerEndpoint MacOSDockerEndpoint `yaml:"docker_endpoint"`
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
	// Resolve library defaults in the provider constructor. Parsing configuration
	// must not maintain its own catalog of images or supported capabilities.
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
