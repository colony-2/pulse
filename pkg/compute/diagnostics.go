package compute

import "context"

// SubmissionDiagnostics contains operator observations taken before transport
// normalization. These results are never authoritative admission decisions.
// A nil OriginalResults means the transport supplied no separate observations.
type SubmissionDiagnostics struct {
	OriginalResults []Submission
	CaptureError    *string
}

// DiagnosticSubmitter optionally preserves original provider responses alongside
// the normal Submit result. Observations and method errors have batch scope.
type DiagnosticSubmitter interface {
	SubmitWithDiagnostics(context.Context, []Launch) ([]Submission, SubmissionDiagnostics, error)
}
