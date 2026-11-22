package providerutil

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
)

// Capabilities validates a discovery response before it can exclude work.
func Capabilities(c provider.Capabilities) error {
	seen := map[string]bool{}
	for _, b := range c.Bases {
		if bases.Validate(b.Name) != nil || strings.Contains(b.Name, "*") || seen[b.Name] {
			return fmt.Errorf("invalid or duplicate advertised base")
		}
		seen[b.Name] = true
		for _, name := range b.Capabilities {
			if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
				return fmt.Errorf("invalid advertised capability")
			}
		}
	}
	return nil
}

// DiscoverCapabilities treats failed or invalid discovery as unknown. Return
// the two errors separately for placement diagnostics.
func DiscoverCapabilities(ctx context.Context, p provider.Provider, timeout time.Duration) (provider.Capabilities, error, error) {
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := p.GetCapabilities(call)
	if err != nil {
		return provider.Capabilities{}, err, nil
	}
	if err = Capabilities(c); err != nil {
		return provider.Capabilities{}, nil, err
	}
	return c, nil, nil
}

// Compatible requires a matching advertised base and capability set when a
// catalog is known.
func Compatible(c provider.Capabilities, a provider.Allocation) bool {
	if len(c.Bases) == 0 {
		return !c.BasesComplete
	}
	for _, b := range c.Bases {
		if !bases.Match(a.Base, b.Name) {
			continue
		}
		if !slices.ContainsFunc(a.Capabilities, func(name string) bool { return !slices.Contains(b.Capabilities, name) }) {
			return true
		}
	}
	return false
}
