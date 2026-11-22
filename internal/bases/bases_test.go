package bases

import (
	"errors"
	"testing"
)

func TestSelectorMatching(t *testing.T) {
	for _, tt := range []struct {
		pattern, name string
		want          bool
	}{
		{"ubuntu-26*arm64", "ubuntu-26.04-arm64", true},
		{"ubuntu-26*arm64", "ubuntu-26.04-amd64", false},
		{"*ubuntu*arm64*", "custom-ubuntu-26.04-arm64-debug", true},
		{"ubuntu**arm64", "ubuntu-arm64", true},
		{"ubuntu*", "ubuntu", true}, {"*", "linux/arm64", true},
		{"ubuntu", "ubuntu-26.04-arm64", false}, {"Ubuntu*", "ubuntu-26.04-arm64", false},
		{"ubuntu-26.04-arm64", "ubuntu-26X04-arm64", false},
		{"ubuntu-?", "ubuntu-a", false}, {"ubuntu-[ab]", "ubuntu-a", false},
		{"foo?", "foo?", true}, {"a\\*", "a\\suffix", true},
		{"", "ubuntu", false},
	} {
		if got := Match(tt.pattern, tt.name); got != tt.want {
			t.Errorf("Match(%q,%q)=%v", tt.pattern, tt.name, got)
		}
	}
}
func TestCatalogAndArchitectureConstraints(t *testing.T) {
	for _, tt := range []struct{ selector, platform, want string }{
		{Default, "", "ubuntu-26.04-arm64"},
		{"ubuntu-26.04-amd64", "", "ubuntu-26.04-amd64"},
		{"ubuntu*", "linux/amd64", "ubuntu-26.04-amd64"},
		{"ubuntu*", "linux/arm64", "ubuntu-26.04-arm64"},
		{"linux*", "", ""}, {"ubuntu", "", ""},
		{"ubuntu-26*arm64", "linux/amd64", ""}, {"debian*", "", ""},
	} {
		var platforms []string
		if tt.platform != "" {
			platforms = []string{tt.platform}
		}
		b, err := Resolve(tt.selector, platforms)
		if tt.want == "" {
			if err == nil {
				t.Errorf("accepted unsupported selector %q", tt.selector)
			}
			continue
		}
		if err != nil || b.Name != tt.want || b.Image != Image {
			t.Errorf("Resolve(%q)=%+v,%v", tt.selector, b, err)
		}
	}
}
func TestImageSelectorRejected(t *testing.T) {
	selector := "oci:linux/amd64:docker.io/library/alpine:3"
	if _, err := Resolve(selector, nil); !errors.Is(err, ErrUnsupportedSelector) {
		t.Fatalf("expected unsupported selector, got %v", err)
	}
}
