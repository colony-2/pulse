package httpapi

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sort"
	"sync"

	provider "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/providerutil"
	"github.com/colony-2/pulse/pkg/providerplugin"
)

type providerDetails struct {
	Name          string                      `json:"name"`
	Type          string                      `json:"type"`
	Discovery     string                      `json:"discovery"`
	Bases         []provider.BaseCapabilities `json:"bases"`
	BasesComplete bool                        `json:"bases_complete"`
	Health        *sdk.Health                 `json:"health,omitempty"`
}

func providerHealth(p provider.Provider) *sdk.Health {
	switch h := p.(type) {
	case interface{ Health() sdk.Health }:
		v := h.Health()
		return &v
	case interface{ Health() providerplugin.Health }:
		v := h.Health()
		return &sdk.Health{
			Name: v.Name, Implementation: v.Implementation, Version: v.Version, Protocol: v.Protocol,
			State: v.State, RestartCount: v.Restarts, Generation: uint64(v.Generation), LastFailure: v.LastFailure,
		}
	default:
		return nil
	}
}

func (a *API) providersHealth(w http.ResponseWriter, _ *http.Request) {
	items := []sdk.Health{}
	for _, p := range a.Controller.Providers {
		if h := providerHealth(p); h != nil {
			items = append(items, *h)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	write(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) describeProvider(ctx context.Context, name string, p provider.Provider) providerDetails {
	cfg := a.Controller.Config
	out := providerDetails{
		Name: name, Type: cfg.Providers[name].Type, Discovery: "unknown",
		Bases: []provider.BaseCapabilities{}, Health: providerHealth(p),
	}
	if err := ctx.Err(); err != nil {
		out.Discovery = discoveryFailure(err)
		return out
	}
	caps, err, validationErr := providerutil.DiscoverCapabilities(ctx, p, cfg.Call)
	switch {
	case err != nil:
		out.Discovery = discoveryFailure(err)
	case validationErr != nil:
		out.Discovery = "invalid"
	case caps.BasesComplete || len(caps.Bases) != 0:
		out.Discovery, out.BasesComplete = "known", caps.BasesComplete
		for _, base := range providerutil.FilterBases(caps, cfg.ExcludeBases).Bases {
			base.Capabilities = append([]string{}, base.Capabilities...)
			out.Bases = append(out.Bases, base)
		}
	}
	return out
}

func discoveryFailure(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, sdk.ErrUnavailable), errors.Is(err, sdk.ErrTransport):
		return "unavailable"
	case errors.Is(err, sdk.ErrProtocol):
		return "invalid"
	default:
		return "error"
	}
}

func (a *API) providerCapabilities(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, ok := a.Controller.Providers[name]
	if !ok {
		write(w, http.StatusNotFound, map[string]string{"error": "unknown provider"})
		return
	}
	if !a.enter(w) {
		return
	}
	defer func() { <-a.slots }()
	write(w, http.StatusOK, a.describeProvider(r.Context(), name, p))
}

func (a *API) providers(w http.ResponseWriter, r *http.Request) {
	if !a.enter(w) {
		return
	}
	defer func() { <-a.slots }()
	ctx, cancel := context.WithTimeout(r.Context(), a.Controller.Config.Call)
	defer cancel()

	backends := a.Controller.Providers
	names := slices.Sorted(maps.Keys(backends))
	items := make([]providerDetails, len(names))
	jobs := make(chan int, len(names))
	for i := range names {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(8, len(names)) {
		workers.Go(func() {
			for i := range jobs {
				name := names[i]
				items[i] = a.describeProvider(ctx, name, backends[name])
			}
		})
	}
	workers.Wait()
	write(w, http.StatusOK, map[string]any{"items": items})
}
