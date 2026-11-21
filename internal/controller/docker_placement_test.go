package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
)

type maintainedDocker struct {
	diagnosticProvider
	t          *testing.T
	maintained bool
}

func (p *maintainedDocker) Maintain(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 1100*time.Millisecond {
		p.t.Error("maintenance not bounded by call_timeout")
	}
	p.maintained = true
	return errors.New("maintenance-test-deferred")
}
func (p *maintainedDocker) Submit(ctx context.Context, launches []api.Launch) ([]api.Submission, error) {
	if !p.maintained {
		p.t.Error("maintenance did not precede wrapped submission")
	}
	return p.diagnosticProvider.Submit(ctx, launches)
}

func TestDockerPlacementAndMaintenance(t *testing.T) {
	for _, status := range []api.Status{api.Accepted, api.NoCapacity, api.Unknown, api.Rejected} {
		t.Run(string(status), func(t *testing.T) {
			c, original, lease, logs := diagnosticFixture(t)
			docker := &maintainedDocker{diagnosticProvider: *original, t: t}
			docker.status, docker.err = status, errors.New("submission-test-error")
			if status == api.Accepted {
				docker.reason = ""
			}
			remote := &accept{}
			c.Providers = map[string]api.Provider{"docker": docker, "remote": remote}
			c.Config.Providers = map[string]config.Provider{"docker": {Type: "docker"}, "remote": {Type: "remote"}}
			endpoint := "http://localhost/tenant"
			c.Config.Targets[0].JobDB = endpoint
			c.Config.Targets[0].Services = []config.Service{{Name: "docker", Priority: 1}, {Name: "remote", Priority: 2}}
			list, claim := c.Lister, c.Claimer
			c.Lister = listFunc(func(ctx context.Context, uri, token string, routes []c2j.Route) (c2j.Page, error) {
				if uri != endpoint {
					t.Error("discovery endpoint changed")
				}
				return list.List(ctx, uri, token, routes)
			})
			c.Claimer = claimFunc(func(ctx context.Context, uri string, j c2j.Job, worker string, duration time.Duration, routes []c2j.Route) (*c2j.Claimed, error) {
				if uri != endpoint {
					t.Error("claim endpoint changed")
				}
				return claim.Claim(ctx, uri, j, worker, duration, routes)
			})
			if err := c.Once(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(docker.requests) != 1 || docker.requests[0].Job.JobDB != "http://host.docker.internal/tenant" || docker.requests[0].Lease != "exported-lease-sentinel" {
				t.Fatal("Docker assignment changed", docker.requests)
			}
			wantRemote := 0
			if status == api.NoCapacity {
				wantRemote = 1
			}
			if len(remote.requests) != wantRemote {
				t.Fatal("incorrect fallback", status, remote.requests)
			}
			if wantRemote != 0 && (remote.requests[0].Job.JobDB != endpoint || remote.requests[0].LaunchID != docker.requests[0].LaunchID || remote.requests[0].Job.JobID != docker.requests[0].Job.JobID || remote.requests[0].Lease != docker.requests[0].Lease) {
				t.Fatal("fallback mutated assignment")
			}
			wantRelease := 0
			if status == api.Rejected {
				wantRelease = 1
			}
			if lease.calls != wantRelease {
				t.Fatal("incorrect lease release", status, lease.calls)
			}
			if !strings.Contains(logs.String(), "maintenance-test-deferred") || !strings.Contains(logs.String(), "submission-test-error") || strings.Contains(logs.String(), "exported-lease-sentinel") {
				t.Fatal("diagnostics lost or lease leaked")
			}
		})
	}
}
