package scheduler

import (
	"context"
	"errors"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/placement"
	"reflect"
	"testing"
	"time"
)

func TestAttemptDiagnosticsDiscoveryAndFallback(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "discovery error", true: "malformed capabilities"}[malformed], func(t *testing.T) {
			calls := [][]string{}
			p := &describedProvider{fake: &fake{calls: &calls, name: "first"}, discoveryErr: errors.New("original discovery error")}
			if malformed {
				p.discoveryErr = nil
				p.capabilities = api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "invalid*"}}}
			}
			p.submit = func(ls []api.Launch) ([]api.Submission, error) {
				return []api.Submission{{LaunchID: ls[0].LaunchID, Status: api.NoCapacity, Reason: "occupied reservation"}}, errors.New("raw submission error")
			}
			second := &fake{calls: &calls, name: "second"}
			third := &fake{calls: &calls, name: "unvisited"}
			out, err := New(time.Minute, time.Second).Run(t.Context(), "s", []Service{{"first", 1, p}, {"second", 2, second}, {"third", 3, third}}, jobs(1))
			if err != nil || len(out) != 1 || out[0].Submission.Status != api.Accepted || out[0].Service != "second" {
				t.Fatal(out, err)
			}
			as := out[0].Attempts
			if len(as) != 2 || !as[0].SubmitCalled || as[0].Discovery != "unknown" || as[0].Result.Reason != "occupied reservation" || as[0].OriginalResults[0].Reason != "occupied reservation" || *as[0].SubmissionError != "raw submission error" || as[0].SubmissionErrorScope != "batch" || as[1].Service != "second" {
				t.Fatal(as)
			}
			if malformed {
				if as[0].DiscoveryError != nil || as[0].CapabilityValidationError == nil {
					t.Fatal(as)
				}
			} else if *as[0].DiscoveryError != "original discovery error" || as[0].CapabilityValidationError != nil {
				t.Fatal(as)
			}
			if !reflect.DeepEqual(calls, [][]string{{"first", "a"}, {"second", "a"}}) {
				t.Fatal(calls)
			}
		})
	}
}
func TestAttemptDiagnosticsCompatibilitySkip(t *testing.T) {
	calls := [][]string{}
	p := &describedProvider{fake: &fake{calls: &calls, name: "skip"}, capabilities: api.Capabilities{BasesComplete: true}}
	out, err := New(time.Minute, time.Second).Run(t.Context(), "s", []Service{{"skip", 1, p}}, jobs(1))
	if err != nil || len(calls) != 0 || len(out[0].Attempts) != 1 {
		t.Fatal(out, err, calls)
	}
	a := out[0].Attempts[0]
	if a.SubmitCalled || a.Compatibility != "excluded_by_controller" || len(a.OriginalResults) != 0 || a.SubmissionError != nil || a.Result.Status != api.Unsupported {
		t.Fatal(a)
	}
}
func TestAttemptDiagnosticsPreserveMalformedOriginals(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "invalid", "unrelated"} {
		t.Run(kind, func(t *testing.T) {
			calls := [][]string{}
			p := &fake{calls: &calls, name: "first", submit: func(ls []api.Launch) ([]api.Submission, error) {
				raw := api.Submission{LaunchID: ls[0].LaunchID, Status: api.NoCapacity, Reason: "original reason"}
				switch kind {
				case "missing":
					return nil, errors.New("lost response")
				case "duplicate":
					return []api.Submission{raw, {LaunchID: raw.LaunchID, Status: api.Unavailable, Reason: "second original reason"}}, nil
				case "invalid":
					raw.Status = "bad_status"
				case "unrelated":
					raw.LaunchID = "other-launch"
				}
				return []api.Submission{raw}, errors.New("raw error")
			}}
			js := jobs(1)
			released := false
			js[0].Prepare = func(_ context.Context, l api.Launch) (api.Launch, func(context.Context) error, error) {
				return l, func(context.Context) error { released = true; return nil }, nil
			}
			s := New(time.Minute, time.Second)
			out, err := s.Run(t.Context(), "s", []Service{{"first", 1, p}, {"second", 2, &fake{calls: &calls, name: "second"}}}, js)
			if err != nil || len(calls) != 1 || released || out[0].Submission.Status != api.Unknown || out[0].ExecutionAdmission != "uncertain" || len(s.Cooldowns()) != 0 {
				t.Fatal(out, err, calls, released)
			}
			a := out[0].Attempts[0]
			want := 0
			if kind == "duplicate" {
				want = 2
			}
			if kind == "invalid" {
				want = 1
			}
			if len(a.OriginalResults) != want || a.NormalizationError == nil {
				t.Fatal(a)
			}
			if want > 0 && a.OriginalResults[0].Reason != "original reason" {
				t.Fatal(a)
			}
			if want == 2 && a.OriginalResults[1].Reason != "second original reason" {
				t.Fatal(a)
			}
		})
	}
}
func TestAttemptDiagnosticsBatchErrorIsNotIndividualRejection(t *testing.T) {
	calls := [][]string{}
	p := &fake{calls: &calls, submit: func(ls []api.Launch) ([]api.Submission, error) {
		return []api.Submission{{LaunchID: ls[0].LaunchID, Status: api.Accepted}, {LaunchID: ls[1].LaunchID, Status: api.Unknown, Reason: "uncertain dispatch"}}, errors.New("batch transport problem")
	}}
	out, err := New(time.Minute, time.Second).Run(t.Context(), "s", []Service{{"first", 1, p}}, jobs(2))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range out {
		a := r.Attempts[0]
		if *a.SubmissionError != "batch transport problem" || a.SubmissionErrorScope != "batch" || r.Retry.LocalEligibleAt.State != "unspecified" {
			t.Fatal(r)
		}
		if i == 0 && r.ExecutionAdmission != "accepted" || i == 1 && r.ExecutionAdmission != "uncertain" {
			t.Fatal(r)
		}
	}
}
func TestDetailedPreparationFailureRetainsPartialKnowledge(t *testing.T) {
	calls := [][]string{}
	released := false
	js := jobs(1)
	js[0].PrepareDetailed = func(_ context.Context, l api.Launch) (Prepared, error) {
		return Prepared{Requested: placement.Allocation(l.Allocation), Stage: "exported_lease", Release: func(context.Context) (placement.Release, error) { released = true; return placement.NoRelease(), nil }}, errors.New("original export failure")
	}
	out, err := New(time.Minute, time.Second).Run(t.Context(), "s", []Service{{"p", 1, &fake{calls: &calls}}}, js)
	if err != nil || !released || len(calls) != 0 || out[0].Requested.CPUMillis.Value != int64(1000) || *out[0].Preparation.Error != "original export failure" || out[0].Preparation.Stage != "exported_lease" || len(out[0].Attempts) != 0 {
		t.Fatal(out, err)
	}
}
