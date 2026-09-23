// Package httpapi exposes public, read-only controller diagnostics.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/colony-2/pulse/internal/controller"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/colony-2/pulse/pkg/compute"
)

type API struct {
	Controller *controller.Controller
	Version    string
	StartedAt  time.Time
	config     map[string]any
	// Bound concurrent public listing requests independently of the poll loop.
	slots chan struct{}
}

func New(c *controller.Controller, version string) (*API, error) {
	public, err := c.Config.Public()
	if err != nil {
		return nil, err
	}
	for _, target := range c.Config.Targets {
		services := []scheduler.Service{}
		cfg := append(target.Services[:0:0], target.Services...)
		sort.Slice(cfg, func(i, j int) bool { return cfg[i].Name < cfg[j].Name })
		scope, _ := json.Marshal(cfg)
		for _, service := range cfg {
			services = append(services, scheduler.Service{Name: service.Name, Priority: service.Priority, Provider: c.Providers[service.Name]})
		}
		if err = c.Scheduler.Configure(string(scope), services); err != nil {
			return nil, err
		}
	}
	return &API{Controller: c, Version: version, StartedAt: time.Now().UTC(), config: public, slots: make(chan struct{}, 8)}, nil
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"endpoints": []string{"/status", "/config", "/instances", "/providers/{provider}/instances", "/scheduler/cooldowns", "/scheduler/round-robin"}})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		status := a.Controller.Status()
		state := "starting"
		if status.Passes > 0 {
			state = "ok"
			if !status.LastSucceeded {
				state = "degraded"
			}
		}
		write(w, 200, map[string]any{"status": state, "version": a.Version, "started_at": a.StartedAt, "uptime_seconds": time.Since(a.StartedAt).Seconds(), "providers": len(a.Controller.Providers), "poll": status})
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) { write(w, 200, a.config) })
	mux.HandleFunc("GET /scheduler/cooldowns", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"items": a.Controller.Scheduler.Cooldowns()})
	})
	mux.HandleFunc("GET /scheduler/round-robin", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"items": a.Controller.Scheduler.RoundRobin()})
	})
	mux.HandleFunc("GET /providers/{provider}/instances", a.provider)
	mux.HandleFunc("GET /instances", a.instances)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			write(w, 405, map[string]string{"error": "read-only API"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func query(r *http.Request, all bool) (compute.ListRequest, error) {
	q := compute.ListRequest{}
	for key, values := range r.URL.Query() {
		if len(values) != 1 {
			return q, fmt.Errorf("duplicate query parameter")
		}
		switch key {
		case "launch_id":
			q.LaunchID = values[0]
		case "page_token":
			if all {
				return q, fmt.Errorf("page_token is only supported on provider listing")
			}
			q.PageToken = values[0]
		case "page_size":
			if all {
				return q, fmt.Errorf("page_size is only supported on provider listing")
			}
			n, err := strconv.Atoi(values[0])
			if err != nil || n < 1 {
				return q, fmt.Errorf("invalid page_size")
			}
			q.PageSize = n
		default:
			return q, fmt.Errorf("unknown query parameter")
		}
	}
	return q.Normalize()
}
func (a *API) enter(w http.ResponseWriter) bool {
	select {
	case a.slots <- struct{}{}:
		return true
	default:
		write(w, 429, map[string]string{"error": "too many concurrent listing requests"})
		return false
	}
}
func (a *API) logError(provider string, err error) {
	logger := a.Controller.Log
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("instance listing failed", "provider", provider, "error", err)
}
func (a *API) list(ctx context.Context, p compute.Provider, q compute.ListRequest) (compute.ListResponse, error) {
	page, err := p.List(ctx, q)
	if err != nil {
		return compute.ListResponse{}, err
	}
	if page.Items == nil || len(page.Items) > q.PageSize || len(page.NextPageToken) > 16384 || (page.NextPageToken != "" && page.NextPageToken == q.PageToken) {
		return compute.ListResponse{}, fmt.Errorf("invalid provider list page")
	}
	seen := map[string]bool{}
	for i, item := range page.Items {
		if seen[item.ID] {
			return compute.ListResponse{}, fmt.Errorf("duplicate instance ID")
		}
		seen[item.ID] = true
		if err = item.Validate(); err != nil {
			return compute.ListResponse{}, err
		}
		if q.LaunchID != "" && item.LaunchID != q.LaunchID {
			return compute.ListResponse{}, fmt.Errorf("provider returned an unexpected launch")
		}
		page.Items[i].Metadata = compute.Correlation(item.Metadata)
	}
	return page, nil
}
func (a *API) provider(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, ok := a.Controller.Providers[name]
	if !ok {
		write(w, 404, map[string]string{"error": "unknown provider"})
		return
	}
	q, err := query(r, false)
	if err != nil {
		write(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !a.enter(w) {
		return
	}
	defer func() { <-a.slots }()
	ctx, cancel := context.WithTimeout(r.Context(), a.Controller.Config.Call)
	defer cancel()
	page, err := a.list(ctx, p, q)
	if err != nil {
		a.logError(name, err)
		write(w, 502, map[string]string{"error": "provider listing failed", "provider": name})
		return
	}
	write(w, 200, page)
}

type instance struct {
	Provider string `json:"provider"`
	compute.Instance
}
type providerError struct {
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

func (a *API) instances(w http.ResponseWriter, r *http.Request) {
	q, err := query(r, true)
	if err != nil {
		write(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !a.enter(w) {
		return
	}
	defer func() { <-a.slots }()
	ctx, cancel := context.WithTimeout(r.Context(), a.Controller.Config.Call)
	defer cancel()
	var mu sync.Mutex
	items := []instance{}
	failures := []providerError{}
	// A small worker pool bounds fan-out even with many configured providers.
	names := make(chan string)
	var workers sync.WaitGroup
	for range min(8, len(a.Controller.Providers)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for name := range names {
				local := []instance{}
				seenTokens := map[string]bool{}
				seenIDs := map[string]bool{}
				query := q
				var listErr error
				for pages := 0; ; pages++ {
					if pages >= 1000 || len(local) >= 10000 {
						listErr = fmt.Errorf("listing scan limit reached")
						break
					}
					page, e := a.list(ctx, a.Controller.Providers[name], query)
					if e != nil {
						listErr = e
						break
					}
					for _, item := range page.Items {
						if !seenIDs[item.ID] {
							local = append(local, instance{name, item})
							seenIDs[item.ID] = true
						}
					}
					if page.NextPageToken == "" {
						break
					}
					if seenTokens[page.NextPageToken] {
						listErr = fmt.Errorf("pagination did not advance")
						break
					}
					seenTokens[page.NextPageToken] = true
					query.PageToken = page.NextPageToken
				}
				mu.Lock()
				items = append(items, local...)
				if listErr != nil {
					failures = append(failures, providerError{name, "provider listing failed or scan incomplete"})
				}
				mu.Unlock()
				if listErr != nil {
					a.logError(name, listErr)
				}
			}
		}()
	}
	for name := range a.Controller.Providers {
		select {
		case names <- name:
		case <-ctx.Done():
			mu.Lock()
			failures = append(failures, providerError{name, "listing deadline exceeded"})
			mu.Unlock()
		}
	}
	close(names)
	workers.Wait()
	sort.Slice(items, func(i, j int) bool {
		if items[i].Provider != items[j].Provider {
			return items[i].Provider < items[j].Provider
		}
		return items[i].ID < items[j].ID
	})
	sort.Slice(failures, func(i, j int) bool { return failures[i].Provider < failures[j].Provider })
	code := 200
	if len(failures) > 0 {
		code = 502
	}
	write(w, code, map[string]any{"items": items, "errors": failures, "complete": len(failures) == 0})
}
