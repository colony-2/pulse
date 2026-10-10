package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/colony-2/pulse/pkg/compute"
	"log/slog"
	"sort"
	"sync"
	"time"
)

type Lister interface {
	List(context.Context, string, string) (c2j.Page, error)
}
type Claimer interface {
	Claim(context.Context, string, c2j.Job, string, time.Duration) (*c2j.Claimed, error)
}
type Controller struct {
	Config    *config.Config
	Lister    Lister
	Claimer   Claimer
	Scheduler *scheduler.Scheduler
	Providers map[string]compute.Provider
	Log       *slog.Logger
	pass      int
	statusMu  sync.RWMutex
	status    PollStatus
}

func (c *Controller) Once(ctx context.Context) (passErr error) {
	c.statusMu.Lock()
	c.status.Running = true
	c.status.LastStarted = time.Now().UTC()
	c.statusMu.Unlock()
	defer func() {
		c.statusMu.Lock()
		defer c.statusMu.Unlock()
		c.status.Running = false
		c.status.LastFinished = time.Now().UTC()
		c.status.Passes++
		c.status.LastSucceeded = passErr == nil
		if passErr != nil {
			c.status.FailedPasses++
		}
	}()
	targets := append([]config.Target(nil), c.Config.Targets...)
	if len(targets) == 0 {
		return fmt.Errorf("no tenant targets")
	}
	offset := c.pass % len(targets)
	c.pass++
	targets = append(targets[offset:], targets[:offset]...)
	var errs []error
	for _, provider := range c.Providers {
		if maintenance, ok := provider.(interface{ Maintain(context.Context) error }); ok {
			call, cancel := context.WithTimeout(ctx, c.Config.Call)
			if err := maintenance.Maintain(call); err != nil {
				c.Log.Warn("provider maintenance deferred", "error", err)
			}
			cancel()
		}
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		serviceCfg := append([]config.Service{}, t.Services...)
		sort.Slice(serviceCfg, func(i, j int) bool { return serviceCfg[i].Name < serviceCfg[j].Name })
		scopeKey, _ := json.Marshal(serviceCfg)
		services := []scheduler.Service{}
		for _, s := range serviceCfg {
			provider := dockerJobDBEndpoint(c.Config.Providers[s.Name].Type, c.Providers[s.Name], t.JobDB, c.Log)
			services = append(services, scheduler.Service{Name: s.Name, Priority: s.Priority, Provider: provider})
		}
		jobs := []scheduler.Job{}
		token := ""
		tokens := map[string]bool{}
		selected := map[scheduler.Key]bool{}
		for pageNumber := 0; pageNumber < c.Config.MaxPages; pageNumber++ {
			call, cancel := context.WithTimeout(ctx, c.Config.Call)
			page, e := c.Lister.List(call, t.JobDB, token)
			cancel()
			if e != nil {
				c.Log.Error("discovery failed", "tenant", t.Tenant, "error", e)
				errs = append(errs, e)
				break
			}
			for _, j := range page.Jobs {
				if !j.Ready(c.Config.Defaults.Routes, time.Now()) {
					continue
				}
				k := scheduler.Key{Instance: t.Instance, Tenant: j.Tenant, Job: j.ID}
				if j.Tenant != t.Tenant {
					c.Log.Error("tenant mismatch", "job", j.ID)
					errs = append(errs, fmt.Errorf("tenant mismatch"))
					continue
				}
				if selected[k] || !c.Scheduler.Eligible(k) {
					continue
				}
				a, e := j.Allocation(c.Config.Allocation)
				if e != nil {
					c.Log.Error("invalid execution demand", "job", j.ID, "error", e)
					errs = append(errs, e)
					continue
				}
				id := compute.NewID()
				metadata := map[string]string{"pulse_metadata_version": "1", "pulse_managed_by": "pulse", "pulse_jobdb_instance_id": t.Instance, "pulse_tenant_id": j.Tenant, "pulse_job_id": j.ID, "pulse_launch_id": id}
				req := compute.Request{LaunchID: id, Allocation: a, TimeoutSeconds: int64(c.Config.ExecutionTimeout / time.Second), Metadata: metadata}
				if c.Config.StartWindow > 0 {
					deadline := time.Now().Add(c.Config.StartWindow)
					req.StartBefore = &deadline
				}

				process, e := c2j.Process(t.JobDB, j.ID, req.Allocation, c.Config.Defaults.Env, metadata)
				if e != nil {
					errs = append(errs, e)
					continue
				}
				jobs = append(jobs, scheduler.Job{Key: k, Request: req, Process: process, Prepare: func(ctx context.Context, request compute.Request) (compute.Launch, func(context.Context) error, error) {
					if c.Claimer == nil {
						return compute.Launch{}, nil, errors.New("JobDB lease client is required")
					}
					claimed, err := c.Claimer.Claim(ctx, t.JobDB, j, request.LaunchID, c.Config.Lease)
					var release func(context.Context) error
					if claimed != nil {
						release = func(ctx context.Context) error { return claimed.Release(ctx, c.Config.Cool) }
					}
					if err != nil {
						return compute.Launch{}, release, err
					}
					if claimed == nil {
						return compute.Launch{}, nil, scheduler.ErrNotEligible
					}
					allocation, err := claimed.Job.Allocation(c.Config.Allocation)
					if err != nil {
						return compute.Launch{}, release, err
					}
					request.Allocation = allocation
					process, err := c2j.Process(t.JobDB, j.ID, allocation, c.Config.Defaults.Env, request.Metadata)
					if err != nil {
						return compute.Launch{}, release, err
					}
					process.Stdin = compute.SecretInput(claimed.Encoded)
					if process.Stdin == "" {
						return compute.Launch{}, release, errors.New("empty exported lease")
					}
					return compute.Launch{Request: request, Process: process}, release, nil
				}})
				selected[k] = true
				if len(jobs) >= c.Config.PerTenant {
					break
				}
			}
			if len(jobs) >= c.Config.PerTenant || page.Next == "" {
				break
			}
			if tokens[page.Next] || page.Next == token {
				errs = append(errs, fmt.Errorf("c2j pagination token did not advance"))
				break
			}
			tokens[page.Next] = true
			token = page.Next
			if pageNumber == c.Config.MaxPages-1 {
				errs = append(errs, fmt.Errorf("tenant %s exceeded page limit", t.Tenant))
			}
		}
		for start := 0; start < len(jobs); start += c.Config.BatchSize {
			end := min(start+c.Config.BatchSize, len(jobs))
			attempt, cancel := context.WithTimeout(ctx, c.Config.Batch)
			results, e := c.Scheduler.Run(attempt, string(scopeKey), services, jobs[start:end])
			cancel()
			if e != nil {
				errs = append(errs, e)
			}
			for _, r := range results {
				// Provider diagnostics may echo credential-bearing request bodies.
				c.Log.Info("launch result", "instance", r.Key.Instance, "tenant", r.Key.Tenant, "job", r.Key.Job, "launch_id", r.Submission.LaunchID, "service", r.Service, "status", r.Submission.Status)
			}
		}
	}
	return errors.Join(errs...)
}
func (c *Controller) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.Config.Poll)
	defer ticker.Stop()
	for {
		if e := c.Once(ctx); e != nil && ctx.Err() == nil {
			c.Log.Error("poll completed with errors", "error", e)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// PollStatus intentionally excludes raw errors, which can contain credentials.
type PollStatus struct {
	Running       bool      `json:"running"`
	LastStarted   time.Time `json:"last_started"`
	LastFinished  time.Time `json:"last_finished"`
	LastSucceeded bool      `json:"last_succeeded"`
	Passes        uint64    `json:"passes"`
	FailedPasses  uint64    `json:"failed_passes"`
}

func (c *Controller) Status() PollStatus {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}
