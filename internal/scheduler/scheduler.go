package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/placement"
	"github.com/colony-2/pulse/internal/providerutil"
	"github.com/colony-2/pulse/pkg/compute"
)

type Key struct{ Instance, Tenant, Job string }
type Job struct {
	Key     Key
	Request provider.Launch
	// Prepare runs after local admission, immediately before placement. The
	// returned cleanup is called only when execution definitely cannot start.
	// Prepare may run concurrently for different jobs and must honor cancellation.
	// A non-nil Prepare explicitly marks an unprepared candidate: Request holds
	// job identity, while Allocation and Lease need not be populated yet.
	// Without Prepare, Request must already be a complete launch.
	Prepare func(context.Context, provider.Launch) (provider.Launch, func(context.Context) error, error)
	// PrepareDetailed is the controller variant: it retains partial diagnostic
	// knowledge and an exact release receipt independently of submission authority.
	PrepareDetailed func(context.Context, provider.Launch) (Prepared, error)
}

type Prepared struct {
	Launch    provider.Launch
	Release   func(context.Context) (placement.Release, error)
	Requested placement.Requested
	Stage     string
}

var ErrNotEligible = errors.New("job is no longer eligible for a lease")

type Service struct {
	Name     string
	Priority int
	Provider provider.Provider
}
type Result struct {
	Key                Key
	Service            string
	Submission         provider.Submission
	Requested          placement.Requested
	Preparation        placement.Preparation
	Attempts           placement.Attempts
	ExecutionAdmission string
	Retry              placement.Retry
}
type entry struct {
	at     time.Time
	flight bool
}
type Scheduler struct {
	mu               sync.Mutex
	entries          map[Key]entry
	cursors          map[string]uint64
	tiers            map[string]RoundRobinEntry
	Cooldown         time.Duration
	CallTimeout      time.Duration
	ClaimConcurrency int
	ClaimTimeout     time.Duration
	Now              func() time.Time
}

func New(cooldown, callTimeout time.Duration) *Scheduler {
	return &Scheduler{entries: map[Key]entry{}, cursors: map[string]uint64{}, tiers: map[string]RoundRobinEntry{}, Cooldown: cooldown, CallTimeout: callTimeout, ClaimConcurrency: 8, ClaimTimeout: 5 * time.Second, Now: time.Now}
}
func (s *Scheduler) order(scope string, services []Service) ([]Service, error) {
	tiers := map[int][]Service{}
	names := map[string]bool{}
	for _, p := range services {
		if p.Name == "" || p.Priority < 1 || p.Provider == nil || names[p.Name] {
			return nil, fmt.Errorf("invalid or duplicate provider %q", p.Name)
		}
		names[p.Name] = true
		tiers[p.Priority] = append(tiers[p.Priority], p)
	}
	if len(tiers) == 0 {
		return nil, fmt.Errorf("no launch services")
	}
	priorities := []int{}
	for p := range tiers {
		priorities = append(priorities, p)
	}
	sort.Ints(priorities)
	out := []Service{}
	for _, p := range priorities {
		tier := tiers[p]
		sort.Slice(tier, func(i, j int) bool { return tier[i].Name < tier[j].Name })
		out = append(out, tier...)
	}
	return out, nil
}

// Run performs one bounded batch attempt. Unknown outcomes never fall through.
func (s *Scheduler) Run(ctx context.Context, scope string, services []Service, jobs []Job) ([]Result, error) {
	if s.ClaimConcurrency < 1 || s.ClaimConcurrency > provider.MaxBatch || s.ClaimTimeout <= 0 {
		return nil, errors.New("invalid claim limits")
	}
	if len(jobs) > provider.MaxBatch {
		return nil, fmt.Errorf("batch too large")
	}
	ordered, err := s.order(scope, services)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	pending := []Job{}
	s.mu.Lock()
	for k, e := range s.entries {
		if !e.flight && now.Sub(e.at) >= s.Cooldown {
			delete(s.entries, k)
		}
	}
	for _, j := range jobs {
		e, exists := s.entries[j.Key]
		if exists && (e.flight || now.Sub(e.at) < s.Cooldown) {
			continue
		}
		s.entries[j.Key] = entry{now, true}
		if j.Request.LaunchID == "" {
			j.Request.LaunchID = compute.NewID()
		}
		pending = append(pending, j)
	}
	s.mu.Unlock()
	reserved := append([]Job{}, pending...)
	results := map[Key]Result{}
	notEligible := map[Key]bool{}
	deferred := map[Key]bool{}
	diagnostics := map[Key]Result{}
	cleanup := map[Key]func(context.Context) (placement.Release, error){}
	validPending := []Job{}
	for _, prepared := range s.prepare(ctx, pending) {
		j, err := prepared.job, prepared.err
		diagnostics[j.Key] = Result{Requested: prepared.requested, Preparation: placement.Preparation{Stage: prepared.stage, Error: placement.Error(err)}, Attempts: placement.Attempts{}, Retry: placement.NoRetry()}
		if !prepared.started {
			deferred[j.Key] = true
			continue
		}
		if prepared.release != nil {
			cleanup[j.Key] = prepared.release
		}
		if err != nil {
			notEligible[j.Key] = errors.Is(err, ErrNotEligible)
			results[j.Key] = Result{Key: j.Key, Submission: provider.Submission{LaunchID: j.Request.LaunchID, Status: provider.Rejected, Reason: err.Error()}}
		} else {
			validPending = append(validPending, j)
		}
	}
	pending = validPending

	for index := 0; index < len(ordered); index++ {
		if len(pending) == 0 {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if index == 0 || ordered[index-1].Priority != ordered[index].Priority {
			end := index + 1
			for end < len(ordered) && ordered[end].Priority == ordered[index].Priority {
				end++
			}
			tier := append([]Service{}, ordered[index:end]...)
			s.mu.Lock()
			cursorKey := fmt.Sprintf("%s/%d", scope, ordered[index].Priority)
			names := []string{}
			for _, service := range tier {
				names = append(names, service.Name)
			}
			s.tiers[cursorKey] = RoundRobinEntry{Scope: scope, Priority: ordered[index].Priority, Services: names}
			offset := int(s.cursors[cursorKey] % uint64(len(tier)))
			s.cursors[cursorKey]++
			s.mu.Unlock()
			rotated := append(tier[offset:], tier[:offset]...)
			copy(ordered[index:end], rotated)
		}
		p := ordered[index]
		call, cancel := context.WithTimeout(ctx, s.CallTimeout)
		capabilities, discoveryErr := p.Provider.GetCapabilities(call)
		cancel()
		var validationErr error
		if discoveryErr == nil {
			validationErr = providerutil.Capabilities(capabilities)
		}
		if discoveryErr != nil || validationErr != nil {
			// Failed/malformed discovery is unknown, never an empty catalog.
			capabilities = provider.Capabilities{}
		}

		next := []Job{}
		launches := make([]provider.Launch, 0, len(pending))
		launchJobs := map[string]Job{}
		for _, j := range pending {
			d := diagnostics[j.Key]
			discovery := "known"
			if discoveryErr != nil || validationErr != nil {
				discovery = "unknown"
			}
			d.Attempts = append(d.Attempts, placement.Attempt{Service: p.Name, ProviderType: "unknown", ProviderPreparation: placement.UnknownProviderPreparation(), Discovery: discovery, DiscoveryError: placement.Error(discoveryErr), CapabilityValidationError: placement.Error(validationErr), DiscoveryScope: "batch", Compatibility: "not_evaluated", OriginalResults: []provider.Submission{}, Capacity: placement.UnknownCapacity()})
			diagnostics[j.Key] = d
		}
		if ctx.Err() != nil {
			break
		}
		for _, j := range pending {
			d := diagnostics[j.Key]
			a := &d.Attempts[len(d.Attempts)-1]
			a.Compatibility = "not_excluded"
			if !providerutil.Compatible(capabilities, j.Request.Allocation) {
				results[j.Key] = Result{Key: j.Key, Service: p.Name, Submission: provider.Submission{LaunchID: j.Request.LaunchID, Status: provider.Unsupported, Reason: "provider capabilities do not match requested base or capabilities"}}
				a.Compatibility = "excluded_by_controller"
				a.Result = results[j.Key].Submission
				diagnostics[j.Key] = d
				next = append(next, j)
				continue
			}
			a.SubmitCalled = true
			diagnostics[j.Key] = d
			launches = append(launches, j.Request)
			launchJobs[j.Request.LaunchID] = j
		}

		if len(launches) > 0 {
			c, cancel := context.WithTimeout(ctx, s.CallTimeout)
			var submitted []provider.Submission
			var submitErr error
			var details map[string]placement.SubmissionDetail
			if detailed, ok := p.Provider.(interface {
				SubmitWithDiagnostics(context.Context, []provider.Launch) ([]provider.Submission, map[string]placement.SubmissionDetail, error)
			}); ok {
				submitted, details, submitErr = detailed.SubmitWithDiagnostics(c, launches)
			} else {
				submitted, submitErr = p.Provider.Submit(c, launches)
			}
			cancel()
			subs := map[string]provider.Submission{}
			dups := map[string]bool{}
			for _, r := range submitted {
				if _, ok := subs[r.LaunchID]; ok {
					dups[r.LaunchID] = true
				}
				subs[r.LaunchID] = r
			}
			for _, launch := range launches {
				j := launchJobs[launch.LaunchID]
				d := diagnostics[j.Key]
				a := &d.Attempts[len(d.Attempts)-1]
				a.SubmissionError = placement.Error(submitErr)
				a.SubmissionErrorScope = "batch"
				for _, raw := range submitted {
					if raw.LaunchID == launch.LaunchID {
						a.OriginalResults = append(a.OriginalResults, raw)
					}
				}
				if detail, ok := details[launch.LaunchID]; ok {
					a.OriginalResults = detail.OriginalResults
					a.SubmissionError = detail.Error
					a.SubmissionErrorScope = detail.ErrorScope
					if a.SubmissionErrorScope == "" {
						a.SubmissionErrorScope = "launch"
					}
					a.OriginalResultsError = detail.OriginalResultsError
					a.NormalizationError = detail.NormalizationError
				}
				r, ok := subs[launch.LaunchID]
				if !ok || dups[launch.LaunchID] || !providerutil.ValidSubmission(r) {
					message := "missing or invalid submission result"
					a.NormalizationError = &message
					r = provider.Submission{LaunchID: launch.LaunchID, Status: provider.Unknown, Reason: message}
				}
				a.Result = r
				diagnostics[j.Key] = d
				results[j.Key] = Result{Key: j.Key, Service: p.Name, Submission: r}
				if providerutil.Fallback(r.Status) {
					next = append(next, j)
				}
			}
		}
		pending = next
	}
	out := []Result{}
	var cleanupErrs []error
	for _, j := range reserved {
		if deferred[j.Key] {
			continue
		}
		r, ok := results[j.Key]
		if !ok {
			r = Result{Key: j.Key, Submission: provider.Submission{LaunchID: j.Request.LaunchID, Status: provider.Rejected, Reason: "attempt cancelled before submission"}}
		}
		d := diagnostics[j.Key]
		r.Requested, r.Preparation, r.Attempts, r.Retry = d.Requested, d.Preparation, d.Attempts, d.Retry
		r.ExecutionAdmission = "not_accepted"
		if r.Submission.Status == provider.Accepted {
			r.ExecutionAdmission = "accepted"
		}
		if r.Submission.Status == provider.Unknown {
			r.ExecutionAdmission = "uncertain"
		}
		if release := cleanup[j.Key]; release != nil && r.Submission.Status != provider.Accepted && r.Submission.Status != provider.Unknown {
			call, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.CallTimeout)
			receipt, err := release(call)
			r.Retry.Release = receipt
			if err != nil {
				r.Retry.Release.Error = placement.Error(err)
				cleanupErrs = append(cleanupErrs, errors.New("failed to release unused job lease"))
			}
			cancel()
		}
		results[j.Key] = r
		out = append(out, r)
	}
	// Finalize at the original end-of-attempt point, under the same lock, and
	// copy the exact stored local deadline into the outgoing diagnostics.
	s.mu.Lock()
	for i := range out {
		r := &out[i]
		status := r.Submission.Status
		if status == provider.Accepted || status == provider.Unknown || notEligible[r.Key] {
			delete(s.entries, r.Key)
			continue
		}
		e := s.entries[r.Key]
		e.flight = false
		e.at = s.Now()
		s.entries[r.Key] = e
		r.Retry.ApplyLocal(e.at, s.Cooldown)
	}
	for k := range deferred {
		delete(s.entries, k)
	}
	s.mu.Unlock()
	return out, errors.Join(append(cleanupErrs, ctx.Err())...)
}

type preparedJob struct {
	job       Job
	started   bool
	release   func(context.Context) (placement.Release, error)
	requested placement.Requested
	stage     string
	err       error
}

// prepare bounds concurrency and how long we keep starting claims. In-flight
// calls finish under their normal request timeout. Results retain input order.
func (s *Scheduler) prepare(ctx context.Context, jobs []Job) []preparedJob {
	claimUntil := s.Now().Add(s.ClaimTimeout)
	out := make([]preparedJob, len(jobs))
	for i, j := range jobs {
		out[i].job = j
	}
	var mu sync.Mutex
	next := 0
	var workers sync.WaitGroup
	for range min(s.ClaimConcurrency, len(jobs)) {
		workers.Go(func() {
			for {
				mu.Lock()
				if ctx.Err() != nil || next == len(jobs) || !s.Now().Before(claimUntil) {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				j := jobs[i]
				result := preparedJob{job: j, started: true, requested: placement.UnknownRequest("preparation has not supplied a leased allocation"), stage: "validation"}
				if j.Prepare == nil && j.PrepareDetailed == nil {
					result.requested = placement.Allocation(j.Request.Allocation)
				}
				var err error
				if j.Prepare != nil || j.PrepareDetailed != nil {
					if j.Key.Instance == "" || j.Key.Tenant == "" || j.Key.Job == "" {
						err = errors.New("missing job identity")
					} else {
						err = providerutil.Identity(j.Request)
					}
				} else {
					err = providerutil.Launch(j.Request)
				}
				if err == nil && (j.Prepare != nil || j.PrepareDetailed != nil) {
					call, done := context.WithTimeout(ctx, s.CallTimeout)
					var launch provider.Launch
					if j.PrepareDetailed != nil {
						prepared, prepareErr := j.PrepareDetailed(call, j.Request)
						launch, result.release, result.requested, result.stage, err = prepared.Launch, prepared.Release, prepared.Requested, prepared.Stage, prepareErr
					} else {
						prepared, release, prepareErr := j.Prepare(call, j.Request)
						launch, err = prepared, prepareErr
						result.stage = "preparation"
						if prepareErr == nil {
							result.requested = placement.Allocation(launch.Allocation)
						}
						if release != nil {
							result.release = func(ctx context.Context) (placement.Release, error) {
								receipt := placement.NoRelease()
								receipt.Outcome = "attempted_without_receipt"
								receipt.RequestedUntil = placement.Unknown("cleanup supplies no deadline receipt")
								receipt.AcknowledgedUntil = receipt.RequestedUntil
								err := release(ctx)
								if err != nil {
									receipt.Outcome = "failed"
								}
								return receipt, err
							}
						}
					}
					done()
					if err == nil {
						if launch.LaunchID != j.Request.LaunchID || launch.Job != j.Request.Job {
							err = errors.New("preparation changed launch or job identity")
							result.stage = "validation"
						} else {
							result.job.Request = launch
							err = providerutil.Launch(launch)
							if err != nil {
								result.stage = "validation"
							}
						}
					}
				}
				result.err = err
				out[i] = result
			}
		})
	}
	workers.Wait()
	return out
}

// Eligible is a hint for filling bounded batches; Run reserves atomically again.
func (s *Scheduler) Eligible(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[k]
	return !ok || (!e.flight && s.Now().Sub(e.at) >= s.Cooldown)
}
