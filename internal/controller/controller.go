package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/colony-2/cortex/internal/c2j"
	"github.com/colony-2/cortex/internal/config"
	"github.com/colony-2/cortex/internal/scheduler"
	"github.com/colony-2/cortex/pkg/compute"
	"log/slog"
	"sort"
	"sync"
	"time"
)

type Lister interface {
	List(context.Context, string, string, string) (c2j.Page, error)
}
type Controller struct {
	Config    *config.Config
	Lister    Lister
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
	type scope struct {
		target config.Target
		cell   string
	}
	scopes := []scope{}
	for _, t := range c.Config.Targets {
		for _, cell := range t.Cells {
			scopes = append(scopes, scope{t, cell})
		}
	}
	if len(scopes) == 0 {
		return fmt.Errorf("no scopes")
	}
	offset := c.pass % len(scopes)
	c.pass++
	scopes = append(scopes[offset:], scopes[:offset]...)
	var errs []error
	for _, scope := range scopes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t := scope.target
		serviceCfg := append([]config.Service{}, t.Services...)
		sort.Slice(serviceCfg, func(i, j int) bool { return serviceCfg[i].Name < serviceCfg[j].Name })
		scopeKey, _ := json.Marshal(serviceCfg)
		services := []scheduler.Service{}
		for _, s := range serviceCfg {
			services = append(services, scheduler.Service{Name: s.Name, Priority: s.Priority, Provider: c.Providers[s.Name]})
		}
		jobs := []scheduler.Job{}
		token := ""
		tokens := map[string]bool{}
		selected := map[scheduler.Key]bool{}
		for pageNumber := 0; pageNumber < c.Config.MaxPages; pageNumber++ {
			call, cancel := context.WithTimeout(ctx, c.Config.Call)
			page, e := c.Lister.List(call, t.JobDB, scope.cell, token)
			cancel()
			if e != nil {
				c.Log.Error("discovery failed", "cell", scope.cell, "error", e)
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
				metadata := map[string]string{"cortex_metadata_version": "1", "cortex_managed_by": "cortex", "cortex_jobdb_instance_id": t.Instance, "cortex_tenant_id": j.Tenant, "cortex_job_id": j.ID, "cortex_launch_id": id}
				req := compute.Request{LaunchID: id, Allocation: a, TimeoutSeconds: int64(c.Config.ExecutionTimeout / time.Second), Metadata: metadata}
				if c.Config.StartWindow > 0 {
					deadline := time.Now().Add(c.Config.StartWindow)
					req.StartBefore = &deadline
				}

				process, e := c2j.Process(t.JobDB, j.ID, id, req.Allocation, c.Config.Defaults.Env, metadata)
				if e != nil {
					errs = append(errs, e)
					continue
				}
				jobs = append(jobs, scheduler.Job{Key: k, Request: req, Process: process})
				selected[k] = true
				if len(jobs) >= c.Config.PerCell {
					break
				}
			}
			if len(jobs) >= c.Config.PerCell || page.Next == "" {
				break
			}
			if tokens[page.Next] || page.Next == token {
				errs = append(errs, fmt.Errorf("c2j pagination token did not advance"))
				break
			}
			tokens[page.Next] = true
			token = page.Next
			if pageNumber == c.Config.MaxPages-1 {
				errs = append(errs, fmt.Errorf("cell %s exceeded page limit", scope.cell))
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
				c.Log.Info("launch result", "instance", r.Key.Instance, "tenant", r.Key.Tenant, "job", r.Key.Job, "launch_id", r.Submission.LaunchID, "service", r.Service, "status", r.Submission.Status, "reason", r.Submission.Reason)
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
