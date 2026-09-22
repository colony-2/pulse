package scheduler

import (
	"fmt"
	"sort"
	"time"
)

type CooldownEntry struct {
	Instance    string    `json:"instance"`
	Tenant      string    `json:"tenant"`
	Job         string    `json:"job"`
	AttemptedAt time.Time `json:"attempted_at"`
	EligibleAt  time.Time `json:"eligible_at"`
	InFlight    bool      `json:"in_flight"`
}
type RoundRobinEntry struct {
	Scope       string   `json:"scope"`
	Priority    int      `json:"priority"`
	Services    []string `json:"services"`
	Batches     uint64   `json:"batches"`
	NextService string   `json:"next_service"`
}

// Cooldowns returns a copy without expiring entries or changing scheduling.
func (s *Scheduler) Cooldowns() []CooldownEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []CooldownEntry{}
	now := s.Now()
	for key, e := range s.entries {
		until := e.at.Add(s.Cooldown)
		if !e.flight && !now.Before(until) {
			continue
		}
		out = append(out, CooldownEntry{key.Instance, key.Tenant, key.Job, e.at, until, e.flight})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Instance != b.Instance {
			return a.Instance < b.Instance
		}
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		return a.Job < b.Job
	})
	return out
}

// RoundRobin returns the next starting service without advancing any cursor.
func (s *Scheduler) RoundRobin() []RoundRobinEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []RoundRobinEntry{}
	for key, tier := range s.tiers {
		e := tier
		e.Services = append([]string{}, tier.Services...)
		e.Batches = s.cursors[key]
		e.NextService = e.Services[e.Batches%uint64(len(e.Services))]
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Priority < out[j].Priority
	})
	return out
}

// Configure makes unvisited tiers visible without advancing their cursors.
func (s *Scheduler) Configure(scope string, services []Service) error {
	ordered, err := s.order(scope, services)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < len(ordered); {
		priority := ordered[i].Priority
		names := []string{}
		for i < len(ordered) && ordered[i].Priority == priority {
			names = append(names, ordered[i].Name)
			i++
		}
		key := fmt.Sprintf("%s/%d", scope, priority)
		s.tiers[key] = RoundRobinEntry{Scope: scope, Priority: priority, Services: names}
	}
	return nil
}
