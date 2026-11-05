package providerutil

import (
	"fmt"
	"slices"
	"strings"
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

// Compatible returns false only when discovery proves incompatibility. A
// partial catalog may support other matching bases. An exact listed base has a
// complete capability set even when the enclosing catalog is partial.
func Compatible(c provider.Capabilities, a provider.Allocation) bool {
	exact := false
	for _, b := range c.Bases {
		if !bases.Match(a.Base, b.Name) {
			continue
		}
		exact = exact || a.Base == b.Name
		if !slices.ContainsFunc(a.Capabilities, func(name string) bool { return !slices.Contains(b.Capabilities, name) }) {
			return true
		}
	}
	return !c.BasesComplete && !exact
}
