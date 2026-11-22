package controller

import (
	"context"
	"errors"

	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/providerutil"
	"github.com/colony-2/pulse/internal/scheduler"
)

// The listing is a snapshot. A skipped job can match on a later poll, and a
// claimed job still goes through placement using its leased requirements.
func (c *Controller) matchesListedDemand(ctx context.Context, j c2j.Job, services []scheduler.Service, catalogs map[string]provider.Capabilities) bool {
	allocation, err := j.Allocation(c.Config.Allocation)
	if errors.Is(err, bases.ErrUnsupportedSelector) {
		return false
	}
	if err != nil || len(services) == 0 {
		return true
	}
	for _, service := range services {
		if service.Provider == nil {
			return true
		}
		catalog, ok := catalogs[service.Name]
		if !ok {
			catalog, _, _ = providerutil.DiscoverCapabilities(ctx, service.Provider, c.Config.Call)
			catalogs[service.Name] = catalog
		}
		if providerutil.Compatible(catalog, allocation) {
			return true
		}
	}
	return false
}
