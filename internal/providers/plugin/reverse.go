package plugin

import (
	"context"

	api "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/placement"
)

// Reverse looks up the current session for one provider identity.
type Reverse struct {
	collector sdk.ProviderCollector
	name      string
}

func NewReverse(collector sdk.ProviderCollector, name string) *Reverse {
	return &Reverse{collector: collector, name: name}
}

func (p *Reverse) current() (api.Provider, error) {
	snapshot, err := p.collector.Snapshot()
	if err != nil {
		return nil, err
	}
	for _, entry := range snapshot.Providers {
		if entry.ID == p.name {
			return entry.Provider, nil
		}
	}
	return nil, sdk.ErrUnavailable
}

func (p *Reverse) Health() sdk.Health {
	current, err := p.current()
	if err != nil {
		return sdk.Health{Name: p.name, State: "unavailable"}
	}
	if h, ok := current.(interface{ Health() sdk.Health }); ok {
		health := h.Health()
		health.Name = p.name
		return health
	}
	return sdk.Health{Name: p.name, State: "ready"}
}

func (p *Reverse) GetCapabilities(ctx context.Context) (api.Capabilities, error) {
	current, err := p.current()
	if err != nil {
		return api.Capabilities{}, err
	}
	return current.GetCapabilities(ctx)
}

func (p *Reverse) List(ctx context.Context, request api.ListRequest) (api.ListResponse, error) {
	current, err := p.current()
	if err != nil {
		return api.ListResponse{}, err
	}
	return current.List(ctx, request)
}

func (p *Reverse) GetLaunchLog(ctx context.Context, request api.LaunchLogRequest) (api.LaunchLogResponse, error) {
	current, err := p.current()
	if err != nil {
		return api.LaunchLogResponse{}, err
	}
	if logs, ok := current.(api.ProviderWithLaunchLogs); ok {
		return logs.GetLaunchLog(ctx, request)
	}
	return api.LaunchLogResponse{}, api.ErrLaunchLogUnsupported
}

func (p *Reverse) Submit(ctx context.Context, launches []api.Launch, options api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	current, err := p.current()
	if err != nil {
		return disconnected(launches), nil, nil
	}
	return current.Submit(ctx, launches, options)
}

func (p *Reverse) SubmitWithDiagnostics(ctx context.Context, launches []api.Launch, options api.SubmitOptions) ([]api.Submission, map[string]placement.SubmissionDetail, []api.LogSubscription, error) {
	current, err := p.current()
	if err != nil {
		return disconnected(launches), nil, nil, nil
	}
	if detailed, ok := current.(detailedProvider); ok {
		return submitWithDiagnostics(ctx, detailed, launches, options)
	}
	results, sub, err := current.Submit(ctx, launches, options)
	var subscriptions []api.LogSubscription
	if sub != nil {
		subscriptions = append(subscriptions, sub)
	}
	return results, nil, subscriptions, err
}

func disconnected(launches []api.Launch) []api.Submission {
	results := make([]api.Submission, 0, len(launches))
	for _, launch := range launches {
		results = append(results, api.Submission{LaunchID: launch.LaunchID, Status: api.Unavailable, Reason: "reverse provider is disconnected"})
	}
	return results
}
