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
	submit func([]compute.Launch) ([]compute.Submission, error)
}

func (f *fake) Submit(_ context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	ids := []string{f.name}
	for _, l := range ls {
		ids = append(ids, l.Metadata["job"])
	}
	*f.calls = append(*f.calls, ids)
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
		out = append(out, Job{Key: Key{"db", "tenant", id}, Request: compute.Request{Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1024, ScratchBytes: 1024, Image: "runner:1", Platform: "linux/amd64"}, TimeoutSeconds: 60, Metadata: map[string]string{"job": id}}, Process: compute.Process{Command: []string{"run"}, Env: map[string]string{"cpu": "1"}}})
	}
	return out
}
func TestPartialFallbackPreservesRequestAndProcess(t *testing.T) {
	calls := [][]string{}
	var fallback compute.Launch
	a := &fake{calls: &calls, name: "a", submit: func(ls []compute.Launch) ([]compute.Submission, error) {
		fallback = ls[1]
		return []compute.Submission{{LaunchID: ls[0].LaunchID, Status: compute.Accepted}, {LaunchID: ls[1].LaunchID, Status: compute.NoCapacity}}, nil
	}}
	b := &fake{calls: &calls, name: "b", submit: func(ls []compute.Launch) ([]compute.Submission, error) {
		if len(ls) != 1 || !reflect.DeepEqual(ls[0], fallback) || ls[0].Process.Env["cpu"] != "1" {
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
			a := &fake{calls: &calls, name: "a", submit: func(ls []compute.Launch) ([]compute.Submission, error) {
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
	decline := func(ls []compute.Launch) ([]compute.Submission, error) {
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

func (b *blocking) Submit(ctx context.Context, r []compute.Launch) ([]compute.Submission, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, errors.New("stop")
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

func TestSubmissionErrorDoesNotFallThrough(t *testing.T) {
	calls := [][]string{}
	b := &blocking{entered: make(chan struct{}), release: make(chan struct{})}
	close(b.release)
	s := New(time.Minute, time.Second)
	r, err := s.Run(context.Background(), "scope", []Service{
		{"unavailable", 1, b},
		{"fallback", 2, &fake{calls: &calls, name: "fallback"}},
	}, jobs(2))
	if err != nil || len(r) != 2 || len(calls) != 0 {
		t.Fatal(r, calls, err)
	}
	for _, result := range r {
		if result.Submission.Status != compute.Unknown || result.Service != "unavailable" {
			t.Fatal(result)
		}
	}
}

func TestInvalidJobDoesNotReachProvider(t *testing.T) {
	for _, invalidProcess := range []bool{false, true} {
		batch := jobs(1)
		if invalidProcess {
			batch[0].Process.Command = nil
		} else {
			batch[0].Request.MemoryBytes = 0
		}
		calls := [][]string{}
		s := New(time.Minute, time.Second)
		r, err := s.Run(context.Background(), "scope", []Service{{"p", 1, &fake{calls: &calls, name: "p"}}}, batch)
		if err != nil || len(r) != 1 || r[0].Submission.Status != compute.Rejected || len(calls) != 0 {
			t.Fatal(r, calls, err)
		}
	}
}
