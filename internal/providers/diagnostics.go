package providers

import (
	"context"
	"log/slog"
	"maps"
	"slices"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providerutil"
)

// Report discovered support without claiming that capacity is free or warm.
// Every provider, including linked external implementations, uses the same API.
func logProviders(ctx context.Context, log *slog.Logger, cfg *config.Config, instances map[string]api.Provider) {
	for _, name := range slices.Sorted(maps.Keys(instances)) {
		attrs := []any{"provider", name, "type", cfg.Providers[name].Type}
		capabilities, err := instances[name].GetCapabilities(ctx)
		if err == nil {
			err = providerutil.Capabilities(capabilities)
		}
		if err != nil {
			log.Warn("provider capability discovery failed", "provider", name)
			attrs = append(attrs, "bases_known", false)
		} else {
			labels := make([]string, 0, len(capabilities.Bases))
			for _, base := range capabilities.Bases {
				labels = append(labels, base.Name)
			}
			attrs = append(attrs, "bases", labels, "bases_complete", capabilities.BasesComplete)
		}
		if p, ok := instances[name].(*macProvider); ok {
			attrs = append(attrs, "configured_slots", p.slots)
		}
		log.Info("provider enabled", attrs...)
	}
}
