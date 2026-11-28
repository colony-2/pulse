package controller

import (
	"encoding/json"
	"maps"
	"slices"
	"sort"

	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/scheduler"
)

func (ctl *Controller) ProviderSnapshot() map[string]provider.Provider {
	if ctl.DynamicProviders == nil {
		return ctl.Providers
	}
	backends := ctl.DynamicProviders()
	maps.Copy(backends, ctl.Providers)
	return backends
}

func (ctl *Controller) ProviderType(name string) string {
	if _, static := ctl.Providers[name]; static || ctl.DynamicProviders == nil {
		return ctl.Config.Providers[name].Type
	}
	return "reverse"
}

func (ctl *Controller) LaunchServices(target config.Target, backends map[string]provider.Provider) (string, []scheduler.Service) {
	configured := append([]config.Service{}, target.Services...)
	sort.Slice(configured, func(left, right int) bool { return configured[left].Name < configured[right].Name })
	services := make([]scheduler.Service, 0, len(configured))
	for _, service := range configured {
		services = append(services, scheduler.Service{Name: service.Name, Priority: service.Priority, Provider: backends[service.Name]})
	}
	if target.DynamicProviderPriority == 0 {
		scope, _ := json.Marshal(configured)
		return string(scope), services
	}
	scope, _ := json.Marshal(struct {
		Services                []config.Service
		DynamicProviderPriority int
	}{configured, target.DynamicProviderPriority})
	for _, name := range slices.Sorted(maps.Keys(backends)) {
		if _, static := ctl.Providers[name]; !static {
			services = append(services, scheduler.Service{Name: name, Priority: target.DynamicProviderPriority, Provider: backends[name]})
		}
	}
	return string(scope), services
}
