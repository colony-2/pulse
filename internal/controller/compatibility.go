package controller

import (
	"context"
	"errors"

	"github.com/colony-2/pulse/internal/bases"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/providerutil"
	"github.com/colony-2/pulse/internal/scheduler"
)

// The listing is a snapshot. A skipped job can match on a later poll, and a
// claimed job still goes through placement using its leased requirements.
func (c *Controller) matchesListedDemand(ctx context.Context, j c2j.Job, services []scheduler.Service, catalogs map[string]providerutil.BaseCatalog) bool {
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
			discovered, _, _ := providerutil.DiscoverCapabilities(ctx, service.Provider, c.Config.Call)
			catalog = providerutil.FilterBases(discovered, c.Config.ExcludeBases)
			catalogs[service.Name] = catalog
		}
		if _, ok := catalog.SelectBase(allocation); ok {
			return true
		}
	}
	return false
}
