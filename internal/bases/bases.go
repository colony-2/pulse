// Package bases resolves Pulse's execution base selectors to native images.
package bases

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/colony-2/pulse/pkg/compute"
	"github.com/distribution/reference"
)

const Default = "ubuntu-26*arm64"
const Image = "ghcr.io/colony-2/base2:latest"

type Base struct {
	Name, Image, Platform string
	Aliases               []string
}

// Catalog returns a fresh catalog; callers may filter by their backend's platforms.
func Catalog() []Base {
	return []Base{
		{"ubuntu-26.04-arm64", Image, "linux/arm64", []string{"ubuntu", "linux"}},
		{"ubuntu-26.04-amd64", Image, "linux/amd64", []string{"ubuntu", "linux"}},
	}
}

func Validate(selector string) error {
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
	parts := strings.Split(selector, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return regexp.MustCompile("(?s)^" + strings.Join(parts, ".*") + "$").MatchString(name)
}

// OCI preserves legacy image/platform requests as exact native base identifiers.
// Wildcards are not supported in this compatibility scheme.
func OCI(image, platform string) (string, error) {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("invalid image reference")
	}
	image = reference.TagNameOnly(ref).String()
	if err := (compute.Allocation{Image: image, Platform: platform}).Validate(); err != nil {
		return "", err
	}
	return "oci:" + platform + ":" + image, nil
}

func Resolve(selector string, platforms []string) (Base, error) {
	if err := Validate(selector); err != nil {
		return Base{}, err
	}
	supported := func(p string) bool {
		if len(platforms) == 0 {
			return true
		}
		for _, v := range platforms {
			if p == v {
				return true
			}
		}
		return false
	}
	if strings.HasPrefix(selector, "oci:") {
		platform, image, ok := strings.Cut(strings.TrimPrefix(selector, "oci:"), ":")
		canonical, err := OCI(image, platform)
		if !ok || err != nil || canonical != selector || !supported(platform) {
			return Base{}, fmt.Errorf("unsupported native image base")
		}
		return Base{Name: selector, Image: image, Platform: platform}, nil
	}
	for _, b := range Catalog() {
		if !supported(b.Platform) {
			continue
		}
		for _, name := range append([]string{b.Name}, b.Aliases...) {
			if Match(selector, name) {
				return b, nil
			}
		}
	}
	return Base{}, fmt.Errorf("no supported base matches selector")
}

// Constrain translates c2j's existing platform requirement without broadening it.
func Constrain(selector, platform string) (string, error) {
	b, err := Resolve(selector, []string{platform})
	if err != nil {
		return "", err
	}
	return b.Name, nil
}
