package providerutil

import (
	"context"
	"slices"

	provider "github.com/colony-2/pulse-provider-api"
)

// JoinLogSubscriptions exposes one handle when a provider adapter makes
// multiple native submissions to preserve per-launch admission diagnostics.
func JoinLogSubscriptions(options provider.SubmitOptions, subscriptions []provider.LogSubscription) provider.LogSubscription {
	if options.Listener == nil {
		return nil
	}
	if len(subscriptions) == 1 {
		return subscriptions[0]
	}
	group := &logSubscriptions{items: subscriptions, done: make(chan struct{})}
	if len(subscriptions) == 0 {
		group.result.Reason = provider.ListenerSourceUnavailable
		close(group.done)
		return group
	}
	go func() {
		defer close(group.done)
		group.result.Reason = provider.ListenerSourcesClosed
		for _, sub := range subscriptions {
			result, err := sub.Wait(context.Background())
			group.result.Coverage = append(group.result.Coverage, result.Coverage...)
			if group.result.Reason == provider.ListenerSourcesClosed && result.Reason != provider.ListenerSourcesClosed {
				group.result.Reason, group.result.Cause, group.err = result.Reason, result.Cause, err
			}
		}
	}()
	return group
}

type logSubscriptions struct {
	items  []provider.LogSubscription
	done   chan struct{}
	result provider.ListenerResult
	err    error
}

func (s *logSubscriptions) Stop() {
	for _, sub := range s.items {
		sub.Stop()
	}
}

func (s *logSubscriptions) Done() <-chan struct{} { return s.done }

func (s *logSubscriptions) Wait(ctx context.Context) (provider.ListenerResult, error) {
	select {
	case <-s.done:
		out := s.result
		out.Coverage = slices.Clone(out.Coverage)
		return out, s.err
	case <-ctx.Done():
		return provider.ListenerResult{}, ctx.Err()
	}
}

func (s *logSubscriptions) Coverage() []provider.SourceStatus {
	var out []provider.SourceStatus
	for _, sub := range s.items {
		out = append(out, sub.Coverage()...)
	}
	return out
}
