// Package providerplugin hosts and serves trusted local Pulse instance providers.
package providerplugin

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"github.com/colony-2/pulse/pkg/compute"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

const (
	ProtocolVersion        = 1
	DefaultMaxMessageBytes = 16 << 20
	MaxMessageBytes        = 128 << 20
	MaxConfigBytes         = 1 << 20
	MaxReasonBytes         = 4 << 10
)

var (
	ErrConfig         = errors.New("invalid provider configuration")
	ErrInitialization = errors.New("provider initialization failed")
	ErrUnavailable    = errors.New("provider unavailable")
	ErrProtocol       = errors.New("invalid provider response")
	ErrUncertain      = errors.New("provider submission outcome uncertain")
)

// Init contains only this provider's configuration. Lifetime is independent of
// the factory's initialization context and ends at shutdown or host loss.
type Init struct {
	Name       string
	ConfigYAML []byte
	BaseDir    string
	Lifetime   context.Context
}
type Factory func(context.Context, Init) (compute.Provider, error)
type ServeOptions struct {
	Implementation  string
	Version         string
	Features        []string
	MaxMessageBytes int
	Shutdown        func(context.Context) error
}
type ProcessConfig struct {
	// CallTimeout caps business RPCs; earlier caller deadlines still apply.
	CallTimeout      time.Duration
	Command          string
	Args             []string
	PassEnv          []string
	SHA256           string
	StartTimeout     time.Duration
	ShutdownTimeout  time.Duration
	MaxConcurrent    int
	MaxMessageBytes  int
	Name             string
	ConfigYAML       []byte
	BaseDir          string
	RequiredFeatures []string
}

func defaults(c ProcessConfig) ProcessConfig {
	if c.CallTimeout == 0 {
		c.CallTimeout = 2 * time.Minute
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = 30 * time.Second
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 5 * time.Second
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 4
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if len(c.ConfigYAML) == 0 {
		c.ConfigYAML = []byte("{}\n")
	}
	return c
}
func ValidateConfig(c ProcessConfig) error {
	c = defaults(c)
	if !filepath.IsAbs(c.Command) || c.CallTimeout <= 0 || c.StartTimeout <= 0 || c.ShutdownTimeout <= 0 || c.MaxConcurrent < 1 || c.MaxConcurrent > 1024 || c.MaxMessageBytes < MaxConfigBytes+1024 || c.MaxMessageBytes > MaxMessageBytes {
		return ErrConfig
	}
	st, e := os.Stat(c.Command)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return ErrConfig
	}
	if c.SHA256 != "" {
		b, e := hex.DecodeString(c.SHA256)
		if e != nil || len(b) != 32 {
			return ErrConfig
		}
	}
	for _, a := range c.Args {
		if strings.ContainsRune(a, 0) {
			return ErrConfig
		}
	}
	for _, k := range c.PassEnv {
		if k == "" || strings.ContainsAny(k, "=\x00") || reservedEnv(k) {
			return ErrConfig
		}
	}
	return ValidateYAML(c.ConfigYAML)
}
func reservedEnv(k string) bool {
	return strings.HasPrefix(k, "PLUGIN_") || strings.HasPrefix(k, "PULSE_PLUGIN_")
}

// ValidateYAML rejects aliases, duplicate/non-string mapping keys and multiple
// documents. No shell or environment interpolation is performed.
func ValidateYAML(b []byte) error {
	if len(b) > MaxConfigBytes {
		return ErrConfig
	}
	var n yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(b))
	if d.Decode(&n) != nil || len(n.Content) != 1 || n.Content[0].Kind != yaml.MappingNode {
		return ErrConfig
	}
	var extra yaml.Node
	if d.Decode(&extra) != io.EOF {
		return ErrConfig
	}
	var visit func(*yaml.Node) bool
	visit = func(n *yaml.Node) bool {
		if n.Kind == yaml.AliasNode {
			return false
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || seen[k.Value] {
					return false
				}
				seen[k.Value] = true
			}
		}
		for _, child := range n.Content {
			if !visit(child) {
				return false
			}
		}
		return true
	}
	if !visit(n.Content[0]) {
		return ErrConfig
	}
	return nil
}

// DecodeConfig strictly decodes a single mapping without exposing parser errors.
func DecodeConfig(b []byte, out any) error {
	if ValidateYAML(b) != nil {
		return ErrConfig
	}
	var node yaml.Node
	if yaml.Unmarshal(b, &node) != nil || len(node.Content) != 1 || !yamlTypes(node.Content[0], reflect.TypeOf(out)) {
		return ErrConfig
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if d.Decode(out) != nil {
		return ErrConfig
	}
	return nil
}
func ResolvePath(baseDir, path string) (string, error) {
	if !filepath.IsAbs(baseDir) || path == "" {
		return "", ErrConfig
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(baseDir, path), nil
}
