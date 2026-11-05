package scheduler

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
)

type describedProvider struct {
	*fake
	capabilities api.Capabilities
	discoveryErr error
	discoveries  int
}

func (p *describedProvider) GetCapabilities(context.Context) (api.Capabilities, error) {
	p.discoveries++
	return p.capabilities, p.discoveryErr
}
func TestCapabilityFiltering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caps   api.Capabilities
		err    error
		base   string
		needs  []string
		submit bool
	}{
		{name: "matching complete catalog", caps: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}}}, base: "ubuntu-26*", submit: true},
		{name: "wrong base", caps: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "macos-15-arm64"}}}, base: "ubuntu*"},
		{name: "no bases", caps: api.Capabilities{BasesComplete: true}, base: "*"},
		{name: "missing capability", caps: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}}}, base: "ubuntu*", needs: []string{"docker"}},
		{name: "capable alternative", caps: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-arm64"}, {Name: "ubuntu-26.04-amd64", Capabilities: []string{"docker"}}}}, base: "ubuntu*", needs: []string{"docker"}, submit: true},
		{name: "partial unknown", caps: api.Capabilities{Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}}}, base: "oci:linux/amd64:docker.io/library/alpine:3", submit: true},
		{name: "partial exact missing capability", caps: api.Capabilities{Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}}}, base: "ubuntu-26.04-amd64", needs: []string{"docker"}},
		{name: "partial wildcard unknown alternative", caps: api.Capabilities{Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}}}, base: "ubuntu*", needs: []string{"docker"}, submit: true},
		{name: "failed discovery discards result", caps: api.Capabilities{BasesComplete: true}, err: errors.New("offline"), base: "ubuntu*", submit: true},
		{name: "malformed catalog is unknown", caps: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "macos*"}}}, base: "ubuntu*", submit: true},
		{name: "duplicate catalog is unknown", caps: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "macos-15"}, {Name: "macos-15"}}}, base: "ubuntu*", submit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := [][]string{}
			p := &describedProvider{fake: &fake{calls: &calls, name: "p"}, capabilities: tc.caps, discoveryErr: tc.err}
			js := jobs(2)
			released := 0
			for i := range js {
				js[i].Request.Base = tc.base
				js[i].Request.Capabilities = tc.needs
				js[i].Prepare = func(_ context.Context, l api.Launch) (api.Launch, func(context.Context) error, error) {
					return l, func(context.Context) error { released++; return nil }, nil
				}
			}
			out, err := New(time.Minute, time.Second).Run(t.Context(), "scope", []Service{{Name: "p", Priority: 1, Provider: p}}, js)
			if err != nil || len(out) != 2 || (len(calls) > 0) != tc.submit || p.discoveries != 1 {
				t.Fatal(out, err, calls, p.discoveries)
			}
			for _, r := range out {
				want := api.Unsupported
				if tc.submit {
					want = api.Accepted
				}
				if r.Submission.Status != want {
					t.Fatal(r)
				}
			}
			if tc.submit && released != 0 || !tc.submit && released != 2 {
				t.Fatal("wrong lease release count", released)
			}
		})
	}
}
func TestCapabilitiesUseLeasedDemandAndRefresh(t *testing.T) {
	calls := [][]string{}
	docker := &describedProvider{fake: &fake{calls: &calls, name: "docker"}, capabilities: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}}}}
	tart := &describedProvider{fake: &fake{calls: &calls, name: "tart"}, capabilities: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "macos-15-arm64"}}}}
	services := []Service{{Name: "docker", Priority: 1, Provider: docker}, {Name: "tart", Priority: 2, Provider: tart}}
	s := New(time.Minute, time.Second)
	js := jobs(1)
	js[0].Prepare = func(_ context.Context, l api.Launch) (api.Launch, func(context.Context) error, error) {
		l.Base = "macos*arm64"
		return l, nil, nil
	}
	if _, err := s.Run(t.Context(), "scope", services, js); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"tart", "a"}}) {
		t.Fatal("filtered listing instead of leased demand", calls)
	}
	// A changed support snapshot must not stay cached across passes.
	docker.capabilities.Bases = []api.BaseCapabilities{{Name: "macos-26-arm64"}}
	if _, err := s.Run(t.Context(), "scope", services, js); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"tart", "a"}, {"docker", "a"}}) {
		t.Fatal("stale discovery or priority ignored", calls)
	}
}

func TestFilteredBatchPreservesUncertainSubmissionRules(t *testing.T) {
	calls := [][]string{}
	first := &describedProvider{fake: &fake{calls: &calls, name: "first"}, capabilities: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}}}}
	first.submit = func(ls []api.Launch) ([]api.Submission, error) {
		out := []api.Submission{}
		for _, l := range ls {
			status, reason := api.Accepted, ""
			if l.Job.JobID == "c" {
				status, reason = api.NoCapacity, "busy"
			}
			if l.Job.JobID == "d" {
				status, reason = api.Unknown, "lost reply"
			}
			out = append(out, api.Submission{LaunchID: l.LaunchID, Status: status, Reason: reason})
		}
		return out, errors.New("partial response")
	}
	second := &describedProvider{fake: &fake{calls: &calls, name: "second"}, capabilities: api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-amd64"}, {Name: "macos-15-arm64"}}}}
	js := jobs(4)
	js[0].Request.Base = "macos*"
	second.submit = func(ls []api.Launch) ([]api.Submission, error) {
		out := []api.Submission{}
		for _, l := range ls {
			if l.Job.JobID == "a" && l.Base != "macos*" {
				t.Fatal("selector rewritten during fallback")
			}
			out = append(out, api.Submission{LaunchID: l.LaunchID, Status: api.Accepted})
		}
		return out, nil
	}
	out, err := New(time.Minute, time.Second).Run(t.Context(), "scope", []Service{{Name: "first", Priority: 1, Provider: first}, {Name: "second", Priority: 2, Provider: second}}, js)
	if err != nil || len(out) != 4 {
		t.Fatal(out, err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"first", "b", "c", "d"}, {"second", "a", "c"}}) {
		t.Fatal("bad batch or uncertain launch retried", calls)
	}
	if out[3].Submission.Status != api.Unknown {
		t.Fatal(out)
	}
}
