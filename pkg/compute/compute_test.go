package compute

import (
	"strings"
	"testing"
)

func TestAllocationEvidence(t *testing.T) {
	a := Allocation{CPUMillis: 1000, MemoryBytes: 1024, ScratchBytes: 1024, Platform: "linux/arm64", Image: "runner@sha256:" + strings.Repeat("a", 64)}
	if a.Satisfies(a) == nil {
		t.Fatal("digest request accepted without evidence")
	}
	a.ImageDigest = "sha256:" + strings.Repeat("a", 64)
	if err := a.Satisfies(a); err != nil {
		t.Fatal(err)
	}
	b := a
	b.ImageID = "sha256:" + strings.Repeat("a", 64)
	b.ImageDigest = ""
	if b.Satisfies(a) == nil {
		t.Fatal("config ID used as manifest")
	}
	b = a
	b.MemoryBytes = 512
	if b.Satisfies(a) == nil {
		t.Fatal("insufficient allocation")
	}
}
