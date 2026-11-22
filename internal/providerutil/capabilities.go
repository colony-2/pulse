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

// BaseCatalog tracks the allowed bases and whether provider support was known.
type BaseCatalog struct {
	provider.Capabilities
	exclusions []string
	known      bool
}

// FilterBases preserves provider order and leaves the discovery response intact.
func FilterBases(c provider.Capabilities, exclusions []string) BaseCatalog {
	catalog := BaseCatalog{Capabilities: c, exclusions: exclusions, known: c.BasesComplete || len(c.Bases) > 0}
	if len(exclusions) == 0 {
		return catalog
	}
	catalog.Bases = make([]provider.BaseCapabilities, 0, len(c.Bases))
	for _, b := range c.Bases {
		if catalog.excludes(b.Name) {
			continue
		}
		catalog.Bases = append(catalog.Bases, b)
	}
	return catalog
}

func (c BaseCatalog) excludes(name string) bool {
	return slices.ContainsFunc(c.exclusions, func(pattern string) bool { return bases.Match(pattern, name) })
}

// SelectBase returns a selector safe to submit to the provider. If a wildcard
// can reach an excluded base, submit the first matching allowed concrete base.
func (c BaseCatalog) SelectBase(a provider.Allocation) (string, bool) {
	wildcard := strings.Contains(a.Base, "*")
	if !wildcard && c.excludes(a.Base) {
		return "", false
	}
	concrete := wildcard && slices.ContainsFunc(c.exclusions, func(pattern string) bool {
		return bases.Overlap(a.Base, pattern)
	})
	if len(c.Bases) == 0 {
		return a.Base, !c.known && !concrete
	}
	for _, b := range c.Bases {
		if !bases.Match(a.Base, b.Name) {
			continue
		}
		if !slices.ContainsFunc(a.Capabilities, func(name string) bool { return !slices.Contains(b.Capabilities, name) }) {
			if concrete {
				return b.Name, true
			}
			return a.Base, true
		}
	}
	return "", false
}

// Compatible requires a matching advertised base and capability set when a
// catalog is known.
func Compatible(c provider.Capabilities, a provider.Allocation) bool {
	_, ok := FilterBases(c, nil).SelectBase(a)
	return ok
}
