package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/placement"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/colony-2/pulse/pkg/compute"
	"log/slog"
	"sort"
	"sync"
	"time"
)

type Lister interface {
	List(context.Context, string, string, []c2j.Route) (c2j.Page, error)
}
type Claimer interface {
	Claim(context.Context, string, c2j.Job, string, time.Duration, []c2j.Route) (*c2j.Claimed, error)
}
type Controller struct {
	Config    *config.Config
	Lister    Lister
	Claimer   Claimer
	Scheduler *scheduler.Scheduler
	Providers map[string]provider.Provider
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
	preparation := map[string]placement.ProviderPreparation{}
	catalogs := map[string]provider.Capabilities{}
	for name, provider := range c.Providers {
		preparation[name] = placement.UnknownProviderPreparation()
		if maintenance, ok := provider.(interface{ Maintain(context.Context) error }); ok {
			call, cancel := context.WithTimeout(ctx, c.Config.Call)
			err := maintenance.Maintain(call)
			preparation[name] = placement.ReturnedProviderPreparation(err)
			if err != nil {
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
			page, e := c.Lister.List(call, t.JobDB, token, c.Config.Defaults.Routes)
			cancel()
			if e != nil {
				c.Log.Error("discovery failed", "tenant", t.Tenant, "error", e)
				errs = append(errs, e)
				break
			}
			for _, j := range page.Jobs {
				k := scheduler.Key{Instance: t.Instance, Tenant: j.Tenant, Job: j.ID}
				if j.Tenant != t.Tenant {
					c.Log.Error("tenant mismatch", "job", j.ID)
					errs = append(errs, fmt.Errorf("tenant mismatch"))
					continue
				}
				if selected[k] || !c.Scheduler.Eligible(k) {
					continue
				}
				if j.ID == "" {
					errs = append(errs, errors.New("missing job identity"))
					continue
				}
				if !c.matchesListedDemand(ctx, j, services, catalogs) {
					continue
				}
				id := compute.NewID()
				jobRef := provider.JobRef{JobDB: t.JobDB, InstanceID: t.Instance, TenantID: j.Tenant, JobID: j.ID}
				req := provider.Launch{LaunchID: id, TimeoutSeconds: int64(c.Config.ExecutionTimeout / time.Second), Job: jobRef}
				if c.Config.StartWindow > 0 {
					deadline := time.Now().Add(c.Config.StartWindow)
					req.StartBefore = &deadline
				}

				jobs = append(jobs, scheduler.Job{Key: k, Request: req, PrepareDetailed: func(ctx context.Context, request provider.Launch) (scheduler.Prepared, error) {
					prepared := scheduler.Prepared{Requested: placement.UnknownRequest("lease allocation unavailable"), Stage: "claim"}
					if c.Claimer == nil {
						return prepared, errors.New("JobDB lease client is required")
					}
					claimed, err := c.Claimer.Claim(ctx, t.JobDB, j, request.LaunchID, c.Config.Lease, c.Config.Defaults.Routes)
					if claimed != nil {
						prepared.Release = func(ctx context.Context) (placement.Release, error) {
							receipt, err := claimed.ReleaseWithReceipt(ctx, c.Config.Cool)
							release := placement.Release{Outcome: "failed", RequestedUntil: placement.Known(receipt.RequestedUntil), AcknowledgedUntil: placement.Unknown("JobDB did not acknowledge release"), Error: placement.Error(err)}
							if receipt.Acknowledged {
								release.Outcome = "acknowledged"
								release.AcknowledgedUntil = placement.Known(receipt.RequestedUntil)
							}
							return release, err
						}
					}
					if err != nil {
						return prepared, err
					}
					if claimed == nil {
						return prepared, scheduler.ErrNotEligible
					}
					prepared.Stage = "allocation"
					allocation, snapshot, err := claimed.Job.AllocationWithDiagnostic(c.Config.Allocation)
					prepared.Requested = snapshot
					if err != nil {
						c.Log.Error("invalid leased execution demand", "job", j.ID, "error", err)
						return prepared, err
					}
					request.Allocation = allocation
					request.Lease = provider.Lease(claimed.Encoded)
					prepared.Stage = "exported_lease"
					if request.Lease == "" {
						return prepared, errors.New("empty exported lease")
					}
					prepared.Launch = request
					prepared.Stage = "complete"
					return prepared, nil
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
				for i := range r.Attempts {
					r.Attempts[i].ProviderPreparation = preparation[r.Attempts[i].Service]
					if kind := c.Config.Providers[r.Attempts[i].Service].Type; kind != "" {
						r.Attempts[i].ProviderType = kind
					}
				}
				c.Log.Info("launch result", "instance", r.Key.Instance, "tenant", r.Key.Tenant, "job", r.Key.Job, "launch_id", r.Submission.LaunchID, "service", r.Service, "status", r.Submission.Status,
					"requested", r.Requested, "preparation", r.Preparation, "attempts", r.Attempts, "reason", r.Submission.Reason, "execution_admission", r.ExecutionAdmission, "retry", r.Retry)
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
