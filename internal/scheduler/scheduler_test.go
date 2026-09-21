package scheduler

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/cortex/pkg/compute"
)

type fake struct {
	calls  *[][]string
	name   string
	submit func([]compute.PreparedLaunch) ([]compute.Submission, error)
	cpu    int64
}

func (f *fake) Prepare(_ context.Context, rs []compute.Request) ([]compute.Preparation, error) {
	ids := []string{f.name}
	out := []compute.Preparation{}
	for _, r := range rs {
		ids = append(ids, r.Metadata["job"])
		a := r.Allocation
		if f.cpu > 0 {
			a.CPUMillis = f.cpu
		}
		out = append(out, compute.Preparation{LaunchID: r.LaunchID, Status: compute.Prepared, Plan: &compute.Plan{Token: r.LaunchID, ExpiresAt: time.Now().Add(time.Hour), Allocation: a}})
	}
	*f.calls = append(*f.calls, ids)
	return out, nil
}
func (f *fake) Submit(_ context.Context, ls []compute.PreparedLaunch) ([]compute.Submission, error) {
	if f.submit != nil {
		return f.submit(ls)
	}
	out := []compute.Submission{}
	for _, l := range ls {
		out = append(out, compute.Submission{LaunchID: l.LaunchID, Status: compute.Accepted})
	}
	return out, nil
}
func jobs(n int) []Job {
	out := []Job{}
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		out = append(out, Job{Key: Key{"db", "tenant", id}, Request: compute.Request{Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1024, ScratchBytes: 1024, Image: "runner:1", Platform: "linux/amd64"}, TimeoutSeconds: 60, Metadata: map[string]string{"job": id}}, Build: func(a compute.Allocation) (compute.Process, error) {
			return compute.Process{Command: []string{"run"}, Env: map[string]string{"cpu": fmtCPU(a.CPUMillis)}}, nil
		}})
	}
	return out
}
func fmtCPU(n int64) string {
	if n == 2000 {
		return "2"
	}
	return "1"
}
func TestPartialFallbackAndFreshAllocation(t *testing.T) {
	calls := [][]string{}
	a := &fake{calls: &calls, name: "a", submit: func(ls []compute.PreparedLaunch) ([]compute.Submission, error) {
		return []compute.Submission{{LaunchID: ls[0].LaunchID, Status: compute.Accepted}, {LaunchID: ls[1].LaunchID, Status: compute.NoCapacity}}, nil
	}}
	b := &fake{calls: &calls, name: "b", cpu: 2000, submit: func(ls []compute.PreparedLaunch) ([]compute.Submission, error) {
		if len(ls) != 1 || ls[0].Process.Env["cpu"] != "2" {
			t.Fatalf("wrong fallback: %+v", ls)
		}
		return []compute.Submission{{LaunchID: ls[0].LaunchID, Status: compute.Accepted}}, nil
	}}
	s := New(time.Minute, time.Second)
	r, e := s.Run(context.Background(), "scope", []Service{{"a", 1, a}, {"b", 2, b}}, jobs(2))
	if e != nil || len(r) != 2 {
		t.Fatal(r, e)
	}
	if !reflect.DeepEqual(calls, [][]string{{"a", "a", "b"}, {"b", "b"}}) {
		t.Fatal(calls)
	}
	r, e = s.Run(context.Background(), "scope", []Service{{"a", 1, a}}, jobs(2))
	if e != nil || len(r) != 0 {
		t.Fatal("cooldown", r, e)
	}
}
func TestUnknownPartialAndDuplicateResultsDoNotFallThrough(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "duplicate"}[duplicate], func(t *testing.T) {
			calls := [][]string{}
			a := &fake{calls: &calls, name: "a", submit: func(ls []compute.PreparedLaunch) ([]compute.Submission, error) {
				r := []compute.Submission{{LaunchID: ls[0].LaunchID, Status: compute.NoCapacity}}
				if duplicate {
					r = append(r, compute.Submission{LaunchID: ls[0].LaunchID, Status: compute.Accepted})
				}
				return r, errors.New("lost reply")
			}}
			b := &fake{calls: &calls, name: "b"}
			s := New(time.Minute, time.Second)
			r, _ := s.Run(context.Background(), "x", []Service{{"a", 1, a}, {"b", 2, b}}, jobs(2))
			if r[1].Submission.Status != compute.Unknown {
				t.Fatal(r)
			}
			if duplicate && len(calls) != 1 {
				t.Fatal(calls)
			}
			if !duplicate && len(calls) != 2 {
				t.Fatal(calls)
			}
		})
	}
}
func TestTiersRoundRobin(t *testing.T) {
	s := New(time.Minute, time.Second)
	calls := [][]string{}
	decline := func(ls []compute.PreparedLaunch) ([]compute.Submission, error) {
		out := []compute.Submission{}
		for _, l := range ls {
			out = append(out, compute.Submission{LaunchID: l.LaunchID, Status: compute.NoCapacity})
		}
		return out, nil
	}
	services := []Service{{"b", 1, &fake{calls: &calls, name: "b", submit: decline}}, {"a", 1, &fake{calls: &calls, name: "a", submit: decline}}, {"c", 2, &fake{calls: &calls, name: "c"}}}
	now := time.Now()
	s.Now = func() time.Time { return now }
	for range 2 {
		_, err := s.Run(context.Background(), "scope", services, jobs(1))
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	got := []string{}
	for _, c := range calls {
		got = append(got, c[0])
	}
	if !reflect.DeepEqual(got, []string{"a", "b", "c", "b", "a", "c"}) {
		t.Fatal(got)
	}
}

type blocking struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blocking) Prepare(ctx context.Context, r []compute.Request) ([]compute.Preparation, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, errors.New("stop")
}
func (b *blocking) Submit(context.Context, []compute.PreparedLaunch) ([]compute.Submission, error) {
	panic("unexpected")
}
func TestInFlightExclusion(t *testing.T) {
	s := New(time.Minute, time.Second)
	b := &blocking{entered: make(chan struct{}), release: make(chan struct{})}
	services := []Service{{"p", 1, b}}
	done := make(chan struct{})
	go func() { defer close(done); s.Run(context.Background(), "x", services, jobs(1)) }()
	<-b.entered
	r, err := s.Run(context.Background(), "x", services, jobs(1))
	if err != nil || len(r) != 0 {
		t.Fatal(r, err)
	}
	close(b.release)
	<-done
}
