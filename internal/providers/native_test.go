package providers_test

import (
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
	"github.com/colony-2/pulse/pkg/compute"
	"testing"
)

// Native engine tests use arbitrary processes to inspect limits and lifecycle.
// Provider assignment and lease transport are tested separately through Submit.
func nativeProvider(t *testing.T, p provider.Provider) compute.Provider {
	t.Helper()
	v, ok := p.(interface{ NativeProvider() compute.Provider })
	if !ok {
		t.Fatal("missing native adapter")
	}
	return v.NativeProvider()
}
func nativeAllocation(t *testing.T, a provider.Allocation) compute.Allocation {
	t.Helper()
	b, err := bases.Resolve(a.Base, nil)
	if err != nil {
		t.Fatal(err)
	}
	return compute.Allocation{Image: b.Image, Platform: b.Platform, CPUMillis: a.CPUMillis, MemoryBytes: a.MemoryBytes, ScratchBytes: a.ScratchBytes}
}
