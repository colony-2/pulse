package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/config"
	pluginprovider "github.com/colony-2/pulse/internal/providers/plugin"
)

const dynamicProviderRetention = 3 * 24 * time.Hour

type dynamicRegistry struct {
	mu        sync.Mutex
	path      string
	now       func() time.Time
	lastSeen  map[string]time.Time
	collector sdk.ProviderCollector
}

func openDynamicRegistry(path string, reserved map[string]config.Provider, now func() time.Time) (*dynamicRegistry, error) {
	registry := &dynamicRegistry{path: path, now: now, lastSeen: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return registry, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read dynamic provider registry: %w", err)
	}
	if err := json.Unmarshal(data, &registry.lastSeen); err != nil || registry.lastSeen == nil {
		return nil, fmt.Errorf("invalid dynamic provider registry in %s", path)
	}
	for name, seen := range registry.lastSeen {
		if !dynamicProviderName.MatchString(name) || seen.IsZero() {
			return nil, fmt.Errorf("invalid dynamic provider registry entry in %s", path)
		}
		if _, exists := reserved[name]; exists {
			delete(registry.lastSeen, name)
		}
	}
	pruneDynamicProviders(registry.lastSeen, now())
	return registry, nil
}

func (registry *dynamicRegistry) record(change sdk.ProviderChange) error {
	if len(change.Joined) == 0 && len(change.Left) == 0 {
		return nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := registry.now().UTC()
	next := maps.Clone(registry.lastSeen)
	for _, entry := range change.Joined {
		next[entry.ID] = now
	}
	for _, entry := range change.Left {
		next[entry.ID] = now
	}
	return registry.commit(next)
}

func (registry *dynamicRegistry) checkpoint() error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	snapshot, err := registry.collector.Snapshot()
	if err != nil {
		return nil
	}
	now := registry.now().UTC()
	next := maps.Clone(registry.lastSeen)
	for _, entry := range snapshot.Providers {
		next[entry.ID] = now
	}
	if pruneDynamicProviders(next, now) || len(snapshot.Providers) != 0 {
		return registry.commit(next)
	}
	return nil
}

func pruneDynamicProviders(lastSeen map[string]time.Time, now time.Time) bool {
	changed := false
	for name, seen := range lastSeen {
		if !now.Before(seen.Add(dynamicProviderRetention)) {
			delete(lastSeen, name)
			changed = true
		}
	}
	return changed
}

func (registry *dynamicRegistry) providers() map[string]api.Provider {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	out := make(map[string]api.Provider, len(registry.lastSeen))
	now := registry.now()
	for name, seen := range registry.lastSeen {
		if now.Before(seen.Add(dynamicProviderRetention)) {
			out[name] = pluginprovider.NewReverse(registry.collector, name)
		}
	}
	if len(out) == len(registry.lastSeen) {
		return out
	}
	snapshot, _ := registry.collector.Snapshot()
	for _, entry := range snapshot.Providers {
		if _, registered := registry.lastSeen[entry.ID]; registered && out[entry.ID] == nil {
			out[entry.ID] = pluginprovider.NewReverse(registry.collector, entry.ID)
		}
	}
	return out
}

func (registry *dynamicRegistry) commit(next map[string]time.Time) error {
	if err := writeDynamicRegistry(registry.path, next); err != nil {
		return fmt.Errorf("save dynamic provider registry: %w", err)
	}
	registry.lastSeen = next
	return nil
}

func writeDynamicRegistry(path string, lastSeen map[string]time.Time) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".dynamic-providers-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(lastSeen)
	if err == nil {
		err = file.Sync()
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
