package providerregistry

import (
	"context"
	provider "github.com/colony-2/pulse-provider-api"
	"testing"
)

func TestRegistrationCannotOverrideBuiltinsOrOtherFactories(t *testing.T) {
	t.Cleanup(func() { registry.Lock(); delete(registry.factories, "test-registered"); registry.Unlock() })
	factory := func(context.Context, Options) (provider.Provider, func(), error) { return nil, nil, nil }
	for _, kind := range []string{"docker", "cloudrun", "ecs", "azurejobs", "remote", "plugin", "tart", "orchard", "", "Bad Type"} {
		if Register(kind, factory) == nil {
			t.Fatalf("accepted %q", kind)
		}
	}
	if Register("test-nil", nil) == nil {
		t.Fatal("nil factory accepted")
	}
	if err := Register("test-registered", factory); err != nil {
		t.Fatal(err)
	}
	if Register("test-registered", factory) == nil {
		t.Fatal("duplicate registration accepted")
	}
	if _, ok := Lookup("test-registered"); !ok {
		t.Fatal("registration missing")
	}
}
