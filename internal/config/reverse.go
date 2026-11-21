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

func (c *Config) validateReverse() error {
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
	_, port, err := net.SplitHostPort(c.Reverse.Listen)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 0 || n > 65535 {
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
