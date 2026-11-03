// Package providerutil validates the dependency-free provider API in Pulse.
package providerutil

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/colony-2/c2j/pkg/execution"
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
)

var launchID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func Fallback(s provider.Status) bool {
	return s == provider.NoCapacity || s == provider.Unsupported || s == provider.Unavailable
}
func ValidStatus(s provider.Status) bool {
	return s == provider.Accepted || Fallback(s) || s == provider.Rejected || s == provider.Unknown
}

// ValidSubmission requires a complete per-item decision before permitting fallback.
func ValidSubmission(r provider.Submission) bool {
	return ValidStatus(r.Status) && ((r.Status == provider.Accepted && r.Reason == "") || (r.Status != provider.Accepted && r.Reason != ""))
}
func Allocation(a provider.Allocation) error {
	for _, n := range []int64{a.CPUMillis, a.MemoryBytes, a.ScratchBytes} {
		if n < 0 || n > provider.MaxQuantity {
			return fmt.Errorf("invalid resource quantity")
		}
	}
	if err := bases.Validate(a.Base); err != nil {
		return err
	}
	_, err := (execution.Requirements{Capabilities: a.Capabilities}).Normalize()
	return err
}
func Job(j provider.JobRef) error {
	u, err := url.Parse(j.JobDB)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || j.InstanceID == "" || j.TenantID == "" || j.JobID == "" || strings.Trim(u.Path, "/") != j.TenantID {
		return fmt.Errorf("invalid job reference")
	}
	return nil
}
func Identity(l provider.Launch) error {
	if !launchID.MatchString(l.LaunchID) || l.TimeoutSeconds < 0 || l.TimeoutSeconds > int64(365*24*time.Hour/time.Second) {
		return fmt.Errorf("invalid launch identity or timeout")
	}
	return Job(l.Job)
}
func Launch(l provider.Launch) error {
	if err := Identity(l); err != nil {
		return err
	}
	if err := Allocation(l.Allocation); err != nil {
		return err
	}
	if l.Lease == "" || len(l.Lease) > 1<<20 || !utf8.ValidString(string(l.Lease)) || strings.ContainsRune(string(l.Lease), 0) {
		return fmt.Errorf("invalid exported lease")
	}
	return nil
}
func Launches(ls []provider.Launch) error {
	if len(ls) == 0 || len(ls) > provider.MaxBatch {
		return fmt.Errorf("batch must contain 1..%d launches", provider.MaxBatch)
	}
	seen := map[string]bool{}
	for _, l := range ls {
		if err := Launch(l); err != nil {
			return err
		}
		if seen[l.LaunchID] {
			return fmt.Errorf("duplicate launch ID")
		}
		seen[l.LaunchID] = true
	}
	return nil
}
func Normalize(q provider.ListRequest) (provider.ListRequest, error) {
	if q.PageSize == 0 {
		q.PageSize = 100
	}
	if q.PageSize < 1 || q.PageSize > 100 || len(q.PageToken) > 16384 || (q.LaunchID != "" && !launchID.MatchString(q.LaunchID)) {
		return q, fmt.Errorf("invalid list parameters")
	}
	return q, nil
}
func Instance(i provider.Instance) error {
	if i.ID == "" || !launchID.MatchString(i.LaunchID) || i.Refs == nil {
		return fmt.Errorf("invalid active instance")
	}
	switch i.State {
	case "queued", "starting", "running", "paused", "stopping":
	default:
		return fmt.Errorf("invalid active state")
	}
	for _, ref := range i.Refs {
		if ref == "" {
			return fmt.Errorf("invalid instance reference")
		}
	}
	return Job(i.Job)
}
