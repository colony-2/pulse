// Package bases resolves Pulse's execution base selectors to native images.
package bases

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/colony-2/c2j/pkg/execution"
)

const Default = "ubuntu-26*"
const Image = "ghcr.io/colony-2/base2:latest"

var ErrUnsupportedSelector = errors.New("execution must be selected by base name or wildcard")

type Base struct {
	Name, Image, Platform string
}

// Catalog returns a fresh catalog in platform preference order. With no
// platforms it returns the whole catalog in its default order.
func Catalog(platforms ...string) []Base {
	all := []Base{
		{"ubuntu-26.04-arm64", Image, "linux/arm64"},
		{"ubuntu-26.04-amd64", Image, "linux/amd64"},
	}
	if len(platforms) == 0 {
		return all
	}
	out := []Base{}
	seen := map[string]bool{}
	for _, platform := range platforms {
		for _, b := range all {
			if b.Platform == platform && !seen[b.Name] {
				out = append(out, b)
				seen[b.Name] = true
			}
		}
	}
	return out
}

func Validate(selector string) error {
	if strings.HasPrefix(selector, "oci:") {
		return ErrUnsupportedSelector
	}
	if selector == "" || len(selector) > 4096 || !utf8.ValidString(selector) || strings.ContainsAny(selector, "\x00\r\n\t ") {
		return fmt.Errorf("invalid base selector")
	}
	return nil
}

// Match uses only '*', anywhere, for zero or more characters. All other
// characters are literal. Matching is case-sensitive and covers the whole name.
func Match(selector, name string) bool {
	if Validate(selector) != nil {
		return false
	}
	return execution.MatchBase(selector, name)
}

func Resolve(selector string, platforms []string) (Base, error) {
	if err := Validate(selector); err != nil {
		return Base{}, err
	}
	for _, b := range Catalog(platforms...) {
		if Match(selector, b.Name) {
			return b, nil
		}
	}
	return Base{}, fmt.Errorf("no supported base matches selector")
}
