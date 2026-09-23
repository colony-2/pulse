// Package compute defines Pulse's provider-neutral, batch-only launch contract.
package compute

import (
	"context"
	"crypto/rand"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"regexp"
	"strings"
	"time"
)

const MaxBatch = 100
const MaxQuantity int64 = 9007199254740991

type Status string

const (
	Accepted    Status = "accepted"
	NoCapacity  Status = "no_capacity"
	Unsupported Status = "unsupported"
	Unavailable Status = "unavailable"
	Rejected    Status = "rejected"
	Unknown     Status = "unknown"
)

func (s Status) Fallback() bool { return s == NoCapacity || s == Unsupported || s == Unavailable }
func (s Status) Valid() bool {
	return s == Accepted || s.Fallback() || s == Rejected || s == Unknown
}

type Allocation struct {
	CPUMillis    int64  `json:"cpu_millis" yaml:"cpu_millis"`
	MemoryBytes  int64  `json:"memory_bytes" yaml:"memory_bytes"`
	ScratchBytes int64  `json:"scratch_bytes" yaml:"scratch_bytes"`
	Platform     string `json:"platform" yaml:"platform"`
	Image        string `json:"image" yaml:"image"`
	ImageDigest  string `json:"image_digest,omitempty" yaml:"image_digest,omitempty"`
	ImageID      string `json:"image_id,omitempty" yaml:"image_id,omitempty"`
}

func (a Allocation) Validate() error {
	for name, n := range map[string]int64{"cpu": a.CPUMillis, "memory": a.MemoryBytes, "scratch": a.ScratchBytes} {
		if n <= 0 || n > MaxQuantity {
			return fmt.Errorf("invalid %s allocation %d", name, n)
		}
	}
	if a.Image == "" || strings.ContainsAny(a.Image, " \t\r\n") {
		return errors.New("invalid image reference")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_.-]*(/[a-z0-9][a-z0-9_.-]*)?$`).MatchString(a.Platform) {
		return errors.New("invalid platform")
	}
	if _, err := reference.ParseNormalizedNamed(a.Image); err != nil {
		return fmt.Errorf("invalid image: %w", err)
	}
	if a.ImageDigest != "" {
		if err := digest.Digest(a.ImageDigest).Validate(); err != nil {
			return fmt.Errorf("invalid manifest digest: %w", err)
		}
	}
	return nil
}

// Satisfies validates facts, not only numerical minima. A requested tag retains
// its meaning even if the provider also binds the pull to a verified digest.
func (a Allocation) Satisfies(want Allocation) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.CPUMillis < want.CPUMillis || a.MemoryBytes < want.MemoryBytes || a.ScratchBytes < want.ScratchBytes {
		return errors.New("provider reduced resource allocation")
	}
	if a.Image != want.Image || a.Platform != want.Platform {
		return errors.New("provider changed image or platform")
	}
	if _, digest, ok := strings.Cut(want.Image, "@"); ok && a.ImageDigest != digest {
		return errors.New("provider did not establish required image digest")
	}
	return nil
}

type Request struct {
	LaunchID string `json:"launch_id"`
	Allocation
	TimeoutSeconds int64             `json:"timeout_seconds"`
	StartBefore    *time.Time        `json:"start_before,omitempty"`
	Metadata       map[string]string `json:"metadata"`
}

func (r Request) Validate() error {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(r.LaunchID) {
		return errors.New("invalid launch ID")
	}
	if err := r.Allocation.Validate(); err != nil {
		return err
	}
	if r.TimeoutSeconds <= 0 || r.TimeoutSeconds > int64((365*24*time.Hour)/time.Second) {
		return errors.New("timeout must be positive and at most one year")
	}
	if len(r.Metadata) == 0 {
		return errors.New("metadata is required")
	}
	return nil
}

type Process struct {
	Command    []string          `json:"command"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	WorkingDir string            `json:"working_dir,omitempty"`
}

func (p Process) Validate() error {
	if len(p.Command) == 0 || p.Command[0] == "" {
		return errors.New("explicit command required")
	}
	for _, v := range append(append([]string{}, p.Command...), p.Args...) {
		if strings.ContainsRune(v, 0) {
			return errors.New("NUL in command")
		}
	}
	for k, v := range p.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return errors.New("invalid environment entry")
		}
	}
	if p.WorkingDir != "" && !strings.HasPrefix(p.WorkingDir, "/") {
		return errors.New("working directory must be absolute")
	}
	return nil
}

// Launch is a complete immutable submission, reused unchanged on fallback.
type Launch struct {
	Request
	Process Process `json:"process"`
}

func (l Launch) Validate() error {
	if err := l.Request.Validate(); err != nil {
		return err
	}
	return l.Process.Validate()
}
func ValidateLaunches(ls []Launch) error {
	if len(ls) == 0 || len(ls) > MaxBatch {
		return fmt.Errorf("batch must contain 1..%d launches", MaxBatch)
	}
	ids := map[string]bool{}
	for _, l := range ls {
		if err := l.Validate(); err != nil {
			return err
		}
		if ids[l.LaunchID] {
			return errors.New("duplicate launch ID")
		}
		ids[l.LaunchID] = true
	}
	return nil
}

type Submission struct {
	LaunchID string `json:"launch_id"`
	Status   Status `json:"status"`
	Reason   string `json:"reason,omitempty"`
}
type Provider interface {
	Submit(context.Context, []Launch) ([]Submission, error)
	List(context.Context, ListRequest) (ListResponse, error)
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func ValidateBatch(reqs []Request) error {
	if len(reqs) == 0 || len(reqs) > MaxBatch {
		return fmt.Errorf("batch must contain 1..%d requests", MaxBatch)
	}
	seen := map[string]bool{}
	for _, r := range reqs {
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r.LaunchID] {
			return errors.New("duplicate launch ID")
		}
		seen[r.LaunchID] = true
	}
	return nil
}
