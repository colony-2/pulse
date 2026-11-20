package config

import (
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"time"

	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/pkg/providerplugin"
	"gopkg.in/yaml.v3"
)

type PluginConfig struct {
	Endpoint        string    `yaml:"endpoint,omitempty"`
	TokenEnv        string    `yaml:"token_env,omitempty"`
	AllowHTTP       bool      `yaml:"allow_http,omitempty"`
	Protocol        int       `yaml:"protocol,omitempty"`
	Command         string    `yaml:"command,omitempty"`
	Args            []string  `yaml:"args,omitempty"`
	PassEnv         []string  `yaml:"pass_env,omitempty"`
	SHA256          string    `yaml:"sha256,omitempty"`
	StartTimeout    string    `yaml:"start_timeout,omitempty"`
	ShutdownTimeout string    `yaml:"shutdown_timeout,omitempty"`
	MaxConcurrent   int       `yaml:"max_concurrent,omitempty"`
	MaxMessageBytes int       `yaml:"max_message_bytes,omitempty"`
	Config          yaml.Node `yaml:"config,omitempty"`
}

func (p *PluginConfig) ProtocolVersion() int {
	if p.Protocol != 0 {
		return p.Protocol
	}
	if runtime.GOOS != "linux" {
		return providerplugin.ProtocolVersion
	}
	return sdk.Protocol
}

func (p *PluginConfig) ProcessConfig(name, base string) (providerplugin.ProcessConfig, error) {
	if p == nil {
		return providerplugin.ProcessConfig{}, providerplugin.ErrConfig
	}
	if p.Endpoint != "" || p.TokenEnv != "" || p.AllowHTTP {
		return providerplugin.ProcessConfig{}, providerplugin.ErrConfig
	}
	version := p.ProtocolVersion()
	if version != providerplugin.ProtocolVersion && version != sdk.Protocol {
		return providerplugin.ProcessConfig{}, providerplugin.ErrConfig
	}
	c := providerplugin.ProcessConfig{Name: name, BaseDir: base, Command: p.Command, Args: p.Args, PassEnv: p.PassEnv, SHA256: p.SHA256, MaxConcurrent: p.MaxConcurrent, MaxMessageBytes: p.MaxMessageBytes}
	var e error
	if p.StartTimeout != "" {
		c.StartTimeout, e = time.ParseDuration(p.StartTimeout)
		if e != nil || c.StartTimeout <= 0 {
			return c, providerplugin.ErrConfig
		}
	}
	if p.ShutdownTimeout != "" {
		c.ShutdownTimeout, e = time.ParseDuration(p.ShutdownTimeout)
		if e != nil || c.ShutdownTimeout <= 0 {
			return c, providerplugin.ErrConfig
		}
	}
	c.ConfigYAML = []byte("{}\n")
	if p.Config.Kind != 0 {
		c.ConfigYAML, e = yaml.Marshal(&p.Config)
		if e != nil {
			return c, providerplugin.ErrConfig
		}
	}
	if e = providerplugin.ValidateConfig(c); e != nil {
		return c, e
	}
	if version == sdk.Protocol {
		for _, key := range p.PassEnv {
			if key == "TMPDIR" {
				return c, providerplugin.ErrConfig
			}
		}
	}
	return c, nil
}

func (p *PluginConfig) ValidateEndpoint() error {
	if p == nil {
		return providerplugin.ErrConfig
	}
	rest := *p
	rest.Endpoint, rest.TokenEnv, rest.AllowHTTP = "", "", false
	if !reflect.DeepEqual(rest, PluginConfig{}) || p.TokenEnv == "" || strings.ContainsAny(p.TokenEnv, "=\x00") {
		return providerplugin.ErrConfig
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(p.Endpoint, "#") ||
		(u.Scheme != "https" && !(p.AllowHTTP && u.Scheme == "http")) {
		return providerplugin.ErrConfig
	}
	return nil
}

func validatePlugin(p Provider) error {
	if p.Type != "plugin" {
		if p.Plugin != nil {
			return providerplugin.ErrConfig
		}
		return nil
	}
	rest := p
	rest.Type = ""
	rest.Plugin = nil
	if !reflect.DeepEqual(rest, Provider{}) {
		return providerplugin.ErrConfig
	}
	if p.Plugin != nil && p.Plugin.Endpoint != "" {
		return p.Plugin.ValidateEndpoint()
	}
	_, e := p.Plugin.ProcessConfig("", "")
	return e
}

// UnmarshalYAML keeps host-owned settings strict even where yaml.v3 would
// ordinarily coerce numeric or boolean scalars into strings. Config remains a
// node so only the provider's own decoder interprets its schema.
func (p *PluginConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return providerplugin.ErrConfig
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Tag != "!!str" || seen[key.Value] {
			return providerplugin.ErrConfig
		}
		seen[key.Value] = true
		switch key.Value {
		case "command", "sha256", "start_timeout", "shutdown_timeout", "endpoint", "token_env":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return providerplugin.ErrConfig
			}
		case "protocol", "max_concurrent", "max_message_bytes":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
				return providerplugin.ErrConfig
			}
		case "allow_http":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
				return providerplugin.ErrConfig
			}
		case "args", "pass_env":
			if value.Kind != yaml.SequenceNode {
				return providerplugin.ErrConfig
			}
			for _, v := range value.Content {
				if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
					return providerplugin.ErrConfig
				}
			}
		case "config":
			if value.Kind != yaml.MappingNode {
				return providerplugin.ErrConfig
			}
		default:
			return providerplugin.ErrConfig
		}
	}
	if seen["endpoint"] {
		for key := range seen {
			switch key {
			case "endpoint", "token_env", "allow_http":
			default:
				return providerplugin.ErrConfig
			}
		}
	} else if seen["token_env"] || seen["allow_http"] {
		return providerplugin.ErrConfig
	}
	type plain PluginConfig
	var out plain
	if n.Decode(&out) != nil {
		return providerplugin.ErrConfig
	}
	*p = PluginConfig(out)
	if seen["endpoint"] && p.Endpoint == "" {
		return providerplugin.ErrConfig
	}
	return nil
}
