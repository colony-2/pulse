// Package providerregistry registers Go providers linked into a Pulse host.
// Provider implementations need only pulse-provider-api; registration belongs
// in the host's composition code, alongside implementation-specific configuration.
package providerregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sync"

	provider "github.com/colony-2/pulse-provider-api"
)

// Options contains per-provider JSON configuration and host defaults. Config and
// Env may contain credentials and must not be logged. BaseDir resolves relative
// paths. Implementations decide which executor environment settings they support.
type Options struct {
	Name, BaseDir string
	Config        json.RawMessage
	Env           map[string]string
}

// Factory constructs a provider. The context bounds initialization only; the
// host invokes cleanup on shutdown, or when later initialization fails. Return
// cleanup even alongside an error if construction allocated resources.
type Factory func(context.Context, Options) (provider.Provider, func(), error)

var registry = struct {
	sync.RWMutex
	factories map[string]Factory
}{factories: map[string]Factory{}}
var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Register adds a provider type before configuration is loaded. Duplicate names
// and built-in types are rejected. It does not start providers or load plugins.
func Register(kind string, factory Factory) error {
	if factory == nil || !kindPattern.MatchString(kind) {
		return fmt.Errorf("invalid provider registration")
	}
	switch kind {
	case "docker", "cloudrun", "ecs", "azurejobs", "remote", "plugin":
		return fmt.Errorf("provider type %q is built in", kind)
	}
	registry.Lock()
	defer registry.Unlock()
	if _, ok := registry.factories[kind]; ok {
		return fmt.Errorf("provider type %q already registered", kind)
	}
	registry.factories[kind] = factory
	return nil
}
func Lookup(kind string) (Factory, bool) {
	registry.RLock()
	defer registry.RUnlock()
	f, ok := registry.factories[kind]
	return f, ok
}
