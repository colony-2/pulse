// Package adapter connects native container engines and the legacy v1 transports
// to Pulse's structured provider API. Process construction stays at this boundary.
package adapter

import (
	"context"
	"fmt"
	"maps"

	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/providerutil"
	"github.com/colony-2/pulse/pkg/compute"
)

type Provider struct {
	Native    compute.Provider
	Platforms []string
	Env       map[string]string
	// Targets supplies endpoints for resources created before endpoint labels existed.
	Targets map[string]string
}

var _ provider.Provider = (*Provider)(nil)

func Metadata(l provider.Launch) map[string]string {
	return map[string]string{"pulse_metadata_version": "1", "pulse_managed_by": "pulse", "pulse_jobdb_instance_id": l.Job.InstanceID, "pulse_tenant_id": l.Job.TenantID, "pulse_job_id": l.Job.JobID, "pulse_launch_id": l.LaunchID, "pulse_jobdb": l.Job.JobDB}
}
func (p *Provider) Submit(ctx context.Context, launches []provider.Launch) ([]provider.Submission, error) {
	if err := providerutil.Launches(launches); err != nil {
		return nil, err
	}
	out := make([]provider.Submission, 0, len(launches))
	native := make([]compute.Launch, 0, len(launches))
	for _, l := range launches {
		// Existing container adapters do not provision Docker/KVM or other optional
		// interfaces. Never turn requested capability names into unverified facts.
		if len(l.Capabilities) > 0 {
			out = append(out, provider.Submission{LaunchID: l.LaunchID, Status: provider.Unsupported, Reason: "native provider does not supply requested capabilities"})
			continue
		}
		b, err := bases.Resolve(l.Base, p.Platforms)
		if err != nil {
			out = append(out, provider.Submission{LaunchID: l.LaunchID, Status: provider.Unsupported, Reason: "no supported base matches selector"})
			continue
		}
		a := compute.Allocation{CPUMillis: l.CPUMillis, MemoryBytes: l.MemoryBytes, ScratchBytes: l.ScratchBytes, Image: b.Image, Platform: b.Platform}
		metadata := Metadata(l)
		process, err := c2j.Process(l.Job.JobDB, l.Job.JobID, a, maps.Clone(p.Env), metadata)
		if err != nil {
			out = append(out, provider.Submission{LaunchID: l.LaunchID, Status: provider.Rejected, Reason: "invalid executor environment configuration"})
			continue
		}
		process.Env["PULSE_EXECUTION_BASE"] = b.Name
		process.Env["C2J_EXECUTION_BASE"] = b.Name
		process.Env["C2J_EXECUTION_CAPABILITIES"] = "[]"
		process.Stdin = compute.SecretInput(l.Lease)
		native = append(native, compute.Launch{Request: compute.Request{LaunchID: l.LaunchID, Allocation: a, TimeoutSeconds: l.TimeoutSeconds, StartBefore: l.StartBefore, Metadata: metadata}, Process: process})
	}
	if len(native) == 0 {
		return out, nil
	}
	results, err := p.Native.Submit(ctx, native)
	for _, r := range results {
		out = append(out, provider.Submission{LaunchID: r.LaunchID, Status: provider.Status(r.Status), Reason: r.Reason})
	}
	return out, err
}
func (p *Provider) List(ctx context.Context, q provider.ListRequest) (provider.ListResponse, error) {
	q, err := providerutil.Normalize(q)
	if err != nil {
		return provider.ListResponse{}, err
	}
	page, err := p.Native.List(ctx, compute.ListRequest{PageSize: q.PageSize, PageToken: q.PageToken, LaunchID: q.LaunchID})
	if err != nil {
		return provider.ListResponse{}, err
	}
	if page.Items == nil || len(page.Items) > q.PageSize || len(page.NextPageToken) > 16384 || (page.NextPageToken != "" && page.NextPageToken == q.PageToken) {
		return provider.ListResponse{}, fmt.Errorf("invalid native list page")
	}
	out := provider.ListResponse{Items: []provider.Instance{}, NextPageToken: page.NextPageToken}
	seen := map[string]bool{}
	for _, i := range page.Items {
		m := i.Metadata
		job := provider.JobRef{JobDB: m["pulse_jobdb"], InstanceID: m["pulse_jobdb_instance_id"], TenantID: m["pulse_tenant_id"], JobID: m["pulse_job_id"]}
		if job.JobDB == "" {
			job.JobDB = p.Targets[job.InstanceID+"\x00"+job.TenantID]
		}
		item := provider.Instance{ID: i.ID, LaunchID: i.LaunchID, State: i.State, Job: job, Refs: i.Refs, CreatedAt: i.CreatedAt, StartedAt: i.StartedAt}
		if providerutil.Instance(item) != nil || seen[item.ID] || (q.LaunchID != "" && q.LaunchID != item.LaunchID) {
			return provider.ListResponse{}, fmt.Errorf("invalid native instance or incomplete job reference")
		}
		seen[item.ID] = true
		out.Items = append(out.Items, item)
	}
	return out, nil
}
func (p *Provider) Maintain(ctx context.Context) error {
	if m, ok := p.Native.(interface{ Maintain(context.Context) error }); ok {
		return m.Maintain(ctx)
	}
	return nil
}

// NativeProvider exposes the underlying process protocol for legacy hosts.
func (p *Provider) NativeProvider() compute.Provider { return p.Native }
