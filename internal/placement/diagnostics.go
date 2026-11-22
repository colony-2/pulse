// Package placement contains operator-only snapshots. It never carries a lease,
// environment, or a complete launch request, and never makes admission decisions.
package placement

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
)

type DiagnosticSubmitter interface {
	SubmitWithDiagnostics(context.Context, []api.Launch, api.SubmitOptions) ([]api.Submission, map[string]SubmissionDetail, []api.LogSubscription, error)
}

type Fact struct {
	State       string `json:"state"`
	Value       any    `json:"value"`
	Explanation string `json:"explanation,omitempty"`
}

func Known(v any) Fact        { return Fact{State: "known", Value: v} }
func Unknown(why string) Fact { return Fact{State: "unknown", Explanation: why} }
func Unspecified() Fact       { return Fact{State: "unspecified"} }

type Requested struct {
	Base         Fact `json:"base"`
	Platform     Fact `json:"platform"`
	Capabilities Fact `json:"capabilities"`
	CPUMillis    Fact `json:"cpu_millis"`
	MemoryBytes  Fact `json:"memory_bytes"`
	ScratchBytes Fact `json:"scratch_bytes"`
}

func UnknownRequest(why string) Requested {
	f := Unknown(why)
	return Requested{f, f, f, f, f, f}
}

// Allocation snapshots only effective allocation fields. Zero in the provider
// contract means unset, not a measured or explicitly requested zero.
func Allocation(a api.Allocation) Requested {
	quantity := func(n int64) Fact {
		if n == 0 {
			return Unspecified()
		}
		return Known(n)
	}
	platform := Unknown("base selector does not determine a platform")
	// Never resolve a wildcard to a preferred catalog entry for diagnostics.
	if !strings.Contains(a.Base, "*") {
		if b, err := bases.Resolve(a.Base, nil); err == nil {
			platform = Known(b.Platform)
		}
	}
	return Requested{Known(a.Base), platform, Known(append([]string{}, a.Capabilities...)), quantity(a.CPUMillis), quantity(a.MemoryBytes), quantity(a.ScratchBytes)}
}

type Preparation struct {
	Stage string  `json:"stage"`
	Error *string `json:"error"`
}

func Error(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	return &s
}

type Capacity struct {
	Occupied            Fact `json:"occupied"`
	AvailableWorkers    Fact `json:"available_workers"`
	ResourceLimits      Fact `json:"resource_limits"`
	AdmissionConstraint Fact `json:"admission_constraint"`
}

func UnknownCapacity() Capacity {
	f := Unknown("provider contract supplies no measured admission snapshot; see original reasons")
	return Capacity{f, f, f, f}
}

// ProviderPreparation records the existing maintenance call's returned error.
// It has provider scope: it does not assert a per-launch refusal or prove that
// preparation succeeded when the call returned no error.
type ProviderPreparation struct {
	Observation string  `json:"observation"`
	Scope       string  `json:"scope"`
	Error       *string `json:"error"`
}

func UnknownProviderPreparation() ProviderPreparation {
	return ProviderPreparation{Observation: "unknown", Scope: "provider"}
}
func ReturnedProviderPreparation(err error) ProviderPreparation {
	observation := "no_error_returned"
	if err != nil {
		observation = "error_returned"
	}
	return ProviderPreparation{Observation: observation, Scope: "provider", Error: Error(err)}
}

type Attempt struct {
	Service                   string              `json:"service"`
	ProviderType              string              `json:"provider_type"`
	ProviderPreparation       ProviderPreparation `json:"provider_preparation"`
	Discovery                 string              `json:"discovery"`
	DiscoveryError            *string             `json:"discovery_error"`
	CapabilityValidationError *string             `json:"capability_validation_error"`
	DiscoveryScope            string              `json:"discovery_scope"`
	Compatibility             string              `json:"compatibility"`
	SubmitCalled              bool                `json:"submit_called"`
	SubmissionError           *string             `json:"submission_error"`
	SubmissionErrorScope      string              `json:"submission_error_scope"`
	OriginalResults           []api.Submission    `json:"original_results"`
	OriginalResultsError      *string             `json:"original_results_error"`
	NormalizationError        *string             `json:"normalization_error"`
	Result                    api.Submission      `json:"result"`
	Capacity                  Capacity            `json:"capacity"`
}

// SubmissionDetail preserves a shim's per-launch runtime observations before
// normalization. Method errors remain separate from original provider Reasons.
type SubmissionDetail struct {
	ErrorScope           string
	OriginalResultsError *string
	OriginalResults      []api.Submission
	Error                *string
	NormalizationError   *string
}

type Release struct {
	Outcome           string  `json:"outcome"`
	RequestedUntil    Fact    `json:"requested_jobdb_until"`
	AcknowledgedUntil Fact    `json:"acknowledged_jobdb_until"`
	Error             *string `json:"error"`
}

func NoRelease() Release {
	return Release{Outcome: "not_attempted", RequestedUntil: Unspecified(), AcknowledgedUntil: Unspecified()}
}

type Retry struct {
	Cooldown        Fact    `json:"cooldown"`
	LocalEligibleAt Fact    `json:"local_eligible_at"`
	Release         Release `json:"release"`
	NextEligibleAt  Fact    `json:"next_eligible_at"`
}

func NoRetry() Retry { return Retry{Unspecified(), Unspecified(), NoRelease(), Unspecified()} }
func (r *Retry) ApplyLocal(at time.Time, cooldown time.Duration) {
	until := at.Add(cooldown).UTC()
	r.Cooldown = Known(cooldown.String())
	r.LocalEligibleAt = Known(until)
	r.NextEligibleAt = Unknown("no acknowledged JobDB deadline")
	if r.Release.Outcome == "acknowledged" {
		if jobdbUntil, ok := r.Release.AcknowledgedUntil.Value.(time.Time); ok {
			if jobdbUntil.After(until) {
				until = jobdbUntil
			}
			r.NextEligibleAt = Known(until)
		}
	}
}

// String keeps nested structures readable under slog's text handler too.
func asJSON(v any) string            { b, _ := json.Marshal(v); return string(b) }
func (r Requested) String() string   { return asJSON(r) }
func (p Preparation) String() string { return asJSON(p) }
func (r Retry) String() string       { return asJSON(r) }

type Attempts []Attempt

func (a Attempts) String() string { return asJSON(a) }
