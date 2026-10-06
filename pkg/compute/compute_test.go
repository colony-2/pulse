package compute

import (
	"encoding/json"
	"fmt"
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

func TestSecretStdinTransportAndRedaction(t *testing.T) {
	p := Process{Command: []string{"c2j"}, Stdin: "private-lease"}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, p), "private-lease") {
			t.Fatal("stdin leaked in diagnostic format", format)
		}
	}
	wire, err := json.Marshal(p)
	if err != nil || !strings.Contains(string(wire), `"stdin":"private-lease"`) {
		t.Fatal("stdin missing from wire")
	}
	p.Env = map[string]string{StdinEnv: "override"}
	if p.Validate() == nil {
		t.Fatal("reserved transport accepted")
	}
	p.Env = nil
	p.Stdin = SecretInput(strings.Repeat("x", MaxStdin+1))
	if p.Validate() == nil {
		t.Fatal("oversized input accepted")
	}
}
