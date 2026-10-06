package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
)

type Key struct{ Instance, Tenant, Job string }
type Job struct {
	Key     Key
	Request compute.Request
	Process compute.Process
	// Prepare runs after local admission, immediately before placement. The
	// returned cleanup is called only when execution definitely cannot start.
	// Prepare may run concurrently for different jobs and must honor cancellation.
	Prepare func(context.Context, compute.Request) (compute.Launch, func(context.Context) error, error)
}

var ErrNotEligible = errors.New("job is no longer eligible for a lease")

type Service struct {
	Name     string
	Priority int
	Provider compute.Provider
}
type Result struct {
	Key        Key
	Service    string
	Submission compute.Submission
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
	if s.ClaimConcurrency < 1 || s.ClaimConcurrency > compute.MaxBatch || s.ClaimTimeout <= 0 {
		return nil, errors.New("invalid claim limits")
	}
	if len(jobs) > compute.MaxBatch {
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
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, j := range reserved {
			status := results[j.Key].Submission.Status
			if status == compute.Accepted || status == compute.Unknown || notEligible[j.Key] || deferred[j.Key] {
				delete(s.entries, j.Key)
				continue
			}
			e := s.entries[j.Key]
			e.flight = false
			e.at = s.Now()
			s.entries[j.Key] = e
		}
	}()
	cleanup := map[Key]func(context.Context) error{}
	validPending := []Job{}
	for _, prepared := range s.prepare(ctx, pending) {
		j, err := prepared.job, prepared.err
		if !prepared.started {
			deferred[j.Key] = true
			continue
		}
		if prepared.release != nil {
			cleanup[j.Key] = prepared.release
		}
		if err != nil {
			notEligible[j.Key] = errors.Is(err, ErrNotEligible)
			results[j.Key] = Result{Key: j.Key, Submission: compute.Submission{LaunchID: j.Request.LaunchID, Status: compute.Rejected, Reason: err.Error()}}
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

		next := []Job{}
		launches := make([]compute.Launch, len(pending))
		launchJobs := map[string]Job{}
		for i, j := range pending {
			launches[i] = compute.Launch{Request: j.Request, Process: j.Process}
			launchJobs[j.Request.LaunchID] = j
		}

		if len(launches) > 0 {
			c, cancel := context.WithTimeout(ctx, s.CallTimeout)
			submitted, submitErr := p.Provider.Submit(c, launches)
			cancel()
			subs := map[string]compute.Submission{}
			dups := map[string]bool{}
			for _, r := range submitted {
				if _, ok := subs[r.LaunchID]; ok {
					dups[r.LaunchID] = true
				}
				subs[r.LaunchID] = r
			}
			for _, launch := range launches {
				j := launchJobs[launch.LaunchID]
				r, ok := subs[launch.LaunchID]
				if !ok || dups[launch.LaunchID] || !r.Status.Valid() {
					r = compute.Submission{LaunchID: launch.LaunchID, Status: compute.Unknown, Reason: fmt.Sprintf("missing or invalid submission result: %v", submitErr)}
				}
				results[j.Key] = Result{j.Key, p.Name, r}
				if r.Status.Fallback() {
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
			r = Result{Key: j.Key, Submission: compute.Submission{LaunchID: j.Request.LaunchID, Status: compute.Rejected, Reason: "attempt cancelled before submission"}}
		}
		results[j.Key] = r
		if release := cleanup[j.Key]; release != nil && r.Submission.Status != compute.Accepted && r.Submission.Status != compute.Unknown {
			call, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.CallTimeout)
			if err := release(call); err != nil {
				cleanupErrs = append(cleanupErrs, errors.New("failed to release unused job lease"))
			}
			cancel()
		}
		out = append(out, r)
	}
	return out, errors.Join(append(cleanupErrs, ctx.Err())...)
}

type preparedJob struct {
	job     Job
	started bool
	release func(context.Context) error
	err     error
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
				result := preparedJob{job: j, started: true}
				err := j.Request.Validate()
				if err == nil {
					err = j.Process.Validate()
				}
				if err == nil && j.Prepare != nil {
					call, done := context.WithTimeout(ctx, s.CallTimeout)
					launch, release, prepareErr := j.Prepare(call, j.Request)
					done()
					result.release = release
					err = prepareErr
					if err == nil {
						if launch.LaunchID != j.Request.LaunchID {
							err = errors.New("preparation changed launch identity")
						} else {
							result.job.Request, result.job.Process = launch.Request, launch.Process
							err = launch.Validate()
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
