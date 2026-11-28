package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type ReverseConfig struct {
	Listen    string `yaml:"listen"`
	Transport string `yaml:"transport"`
	TLSCert   string `yaml:"tls_cert,omitempty"`
	TLSKey    string `yaml:"tls_key,omitempty"`
}

type DynamicProviderConfig struct {
	Listen    string `yaml:"listen"`
	TLSCert   string `yaml:"tls_cert"`
	TLSKey    string `yaml:"tls_key"`
	StateFile string `yaml:"state_file"`
	Auth      struct {
		ClientCA string `yaml:"client_ca"`
	} `yaml:"auth"`
}

func (c *Config) validateReverse() error {
	if dynamic := c.DynamicProvider; dynamic != nil {
		if !validReverseListen(dynamic.Listen) {
			return fmt.Errorf("dynamic_provider.listen must be a host:port address")
		}
		if dynamic.TLSCert == "" || dynamic.TLSKey == "" || dynamic.Auth.ClientCA == "" {
			return fmt.Errorf("dynamic_provider requires tls_cert, tls_key and auth.client_ca")
		}
		if dynamic.StateFile == "" {
			return fmt.Errorf("dynamic_provider requires state_file")
		}
		if len(c.Defaults.Env) != 0 {
			return fmt.Errorf("reverse providers require executor environment to be configured at the provider")
		}
	}
	hasProviders := false
	for name, p := range c.Providers {
		if p.Type != "reverse" {
			continue
		}
		hasProviders = true
		if name == "" || len(name) > 256 || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return fmt.Errorf("invalid reverse provider identity")
		}
		if p.TokenEnv == "" {
			return fmt.Errorf("reverse provider %s requires token_env for its shared key", name)
		}
	}
	if c.Reverse == nil {
		if hasProviders {
			return fmt.Errorf("reverse providers require reverse listener configuration")
		}
		return nil
	}
	if !hasProviders {
		return fmt.Errorf("reverse listener requires at least one reverse provider")
	}
	if len(c.Defaults.Env) != 0 {
		return fmt.Errorf("reverse providers require executor environment to be configured at the provider")
	}
	if !validReverseListen(c.Reverse.Listen) {
		return fmt.Errorf("reverse.listen must be a host:port address")
	}
	if c.Reverse.Transport == "" {
		c.Reverse.Transport = "tls"
	}
	switch c.Reverse.Transport {
	case "tls":
		if c.Reverse.TLSCert == "" || c.Reverse.TLSKey == "" {
			return fmt.Errorf("reverse TLS transport requires tls_cert and tls_key")
		}
	case "http":
		if c.Reverse.TLSCert != "" || c.Reverse.TLSKey != "" {
			return fmt.Errorf("reverse HTTP transport does not accept TLS certificates")
		}
	default:
		return fmt.Errorf("reverse.transport must be tls or http")
	}
	return nil
}

func validReverseListen(address string) bool {
	_, port, err := net.SplitHostPort(address)
	number, portErr := strconv.Atoi(port)
	return err == nil && portErr == nil && number >= 0 && number <= 65535
}
