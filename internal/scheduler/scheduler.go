package scheduler

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/colony-2/cortex/pkg/compute"
)

type Key struct{ Instance, Tenant, Job string }
type Job struct {
	Key     Key
	Request compute.Request
	Build   func(compute.Allocation) (compute.Process, error)
}
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
	mu          sync.Mutex
	entries     map[Key]entry
	cursors     map[string]uint64
	Cooldown    time.Duration
	CallTimeout time.Duration
	Now         func() time.Time
}

func New(cooldown, callTimeout time.Duration) *Scheduler {
	return &Scheduler{entries: map[Key]entry{}, cursors: map[string]uint64{}, Cooldown: cooldown, CallTimeout: callTimeout, Now: time.Now}
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
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, j := range reserved {
			e := s.entries[j.Key]
			e.flight = false
			s.entries[j.Key] = e
		}
	}()
	results := map[Key]Result{}
	validPending := []Job{}
	for _, j := range pending {
		err := j.Request.Validate()
		if err == nil && j.Build == nil {
			err = fmt.Errorf("missing process builder")
		}
		if err != nil {
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
			offset := int(s.cursors[cursorKey] % uint64(len(tier)))
			s.cursors[cursorKey]++
			s.mu.Unlock()
			rotated := append(tier[offset:], tier[:offset]...)
			copy(ordered[index:end], rotated)
		}
		p := ordered[index]
		reqs := make([]compute.Request, len(pending))
		for i, j := range pending {
			reqs[i] = j.Request
		}
		c, cancel := context.WithTimeout(ctx, s.CallTimeout)
		prepared, prepErr := p.Provider.Prepare(c, reqs)
		cancel()
		preps := map[string]compute.Preparation{}
		duplicate := map[string]bool{}
		for _, r := range prepared {
			if _, ok := preps[r.LaunchID]; ok {
				duplicate[r.LaunchID] = true
			}
			preps[r.LaunchID] = r
		}
		next := []Job{}
		launches := []compute.PreparedLaunch{}
		launchJobs := map[string]Job{}
		for _, j := range pending {
			r, ok := preps[j.Request.LaunchID]
			finish := func(status compute.Status, reason string) {
				results[j.Key] = Result{j.Key, p.Name, compute.Submission{LaunchID: j.Request.LaunchID, Status: status, Reason: reason}}
			}
			if !ok && prepErr != nil {
				finish(compute.Unavailable, "provider preparation unavailable")
				next = append(next, j)
				continue
			}
			if !ok || duplicate[j.Request.LaunchID] {
				finish(compute.Rejected, fmt.Sprintf("missing or duplicate preparation result: %v", prepErr))
				continue
			}
			if r.Status.Fallback() {
				finish(r.Status, r.Reason)
				next = append(next, j)
				continue
			}
			if r.Status != compute.Prepared || r.Plan == nil {
				finish(compute.Rejected, r.Reason)
				continue
			}
			if err := r.Plan.Allocation.Satisfies(j.Request.Allocation); err != nil {
				finish(compute.Rejected, err.Error())
				continue
			}
			if r.Plan.Token == "" || !r.Plan.ExpiresAt.After(s.Now()) {
				finish(compute.Rejected, "invalid or expired plan")
				continue
			}
			if j.Build == nil {
				finish(compute.Rejected, "missing process builder")
				continue
			}
			process, err := j.Build(r.Plan.Allocation)
			if err == nil {
				err = process.Validate()
			}
			if err != nil {
				finish(compute.Rejected, err.Error())
				continue
			}
			launches = append(launches, compute.PreparedLaunch{LaunchID: j.Request.LaunchID, Plan: *r.Plan, Process: process})
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
				if !ok || dups[launch.LaunchID] || !r.Status.Valid() || r.Status == compute.Prepared {
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
	for _, j := range reserved {
		r, ok := results[j.Key]
		if !ok {
			r = Result{Key: j.Key, Submission: compute.Submission{LaunchID: j.Request.LaunchID, Status: compute.Rejected, Reason: "attempt cancelled before submission"}}
		}
		out = append(out, r)
	}
	return out, ctx.Err()
}

// Eligible is a hint for filling bounded batches; Run reserves atomically again.
func (s *Scheduler) Eligible(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[k]
	return !ok || (!e.flight && s.Now().Sub(e.at) >= s.Cooldown)
}
