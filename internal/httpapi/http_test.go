package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/cortex/internal/config"
	"github.com/colony-2/cortex/internal/controller"
	"github.com/colony-2/cortex/internal/scheduler"
	"github.com/colony-2/cortex/pkg/compute"
)

type provider struct {
	list func(context.Context, compute.ListRequest) (compute.ListResponse, error)
}

func (p provider) Submit(context.Context, []compute.Launch) ([]compute.Submission, error) {
	panic("read-only endpoint submitted work")
}
func (p provider) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	return p.list(ctx, q)
}
func row(id string) compute.Instance {
	return compute.Instance{ID: id, LaunchID: id, State: "running", Metadata: map[string]string{"cortex_job_id": "job", "secret": "do-not-expose"}, Refs: []string{id}}
}
func newAPI(t *testing.T, providers map[string]compute.Provider) *API {
	t.Helper()
	cfg := &config.Config{Call: time.Second, Cool: time.Minute}
	cfg.Defaults.Env = map[string]string{"TOKEN": "do-not-expose"}
	cfg.Providers = map[string]config.Provider{"p": {Type: "remote", Endpoint: "https://user:do-not-expose@host/path?token=do-not-expose"}}
	api, err := New(&controller.Controller{Config: cfg, Scheduler: scheduler.New(time.Minute, time.Second), Providers: providers, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return api
}
func get(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}
func TestPublicReadOnlyEndpointsAndConfigRedaction(t *testing.T) {
	a := newAPI(t, map[string]compute.Provider{})
	h := a.Handler()
	for _, path := range []string{"/", "/status", "/config", "/instances", "/scheduler/cooldowns", "/scheduler/round-robin"} {
		w := get(h, path)
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), "do-not-expose") {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal("not public")
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != 405 {
			t.Fatal(path, w.Code)
		}
	}
	if w := get(h, "/providers/missing/instances"); w.Code != 404 {
		t.Fatal(w)
	}
	for _, path := range []string{"/instances?page_token=x", "/providers/p/instances?page_size=0"} {
		// Unknown provider is checked before its query parameters.
		w := get(h, path)
		if w.Code != 400 && w.Code != 404 {
			t.Fatal(w)
		}
	}
	if w := get(h, "/config"); !strings.Contains(w.Body.String(), "[REDACTED]") {
		t.Fatal(w.Body.String())
	}
}
func TestAggregatePagesPartialFailureAndProviderSelection(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	good := provider{list: func(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
		mu.Lock()
		calls = append(calls, q.PageToken)
		mu.Unlock()
		if q.PageToken == "next" {
			return compute.ListResponse{Items: []compute.Instance{row("b")}}, nil
		}
		return compute.ListResponse{Items: []compute.Instance{row("a")}, NextPageToken: "next"}, nil
	}}
	bad := provider{list: func(context.Context, compute.ListRequest) (compute.ListResponse, error) {
		return compute.ListResponse{}, errors.New("do-not-expose")
	}}
	a := newAPI(t, map[string]compute.Provider{"good": good, "bad": bad})
	h := a.Handler()
	w := get(h, "/instances")
	if w.Code != 502 || strings.Contains(w.Body.String(), "do-not-expose") {
		t.Fatal(w.Code, w.Body.String())
	}
	var out struct {
		Items    []instance
		Errors   []providerError
		Complete bool
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Complete || len(out.Items) != 2 || len(out.Errors) != 1 || out.Errors[0].Provider != "bad" || !reflect.DeepEqual(calls, []string{"", "next"}) {
		t.Fatal(out, calls)
	}
	w = get(h, "/providers/good/instances?page_token=next&page_size=1")
	if w.Code != 200 || strings.Contains(w.Body.String(), "do-not-expose") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = get(h, "/providers/good/instances?page_size=0"); w.Code != 400 {
		t.Fatal(w.Code)
	}
}
func TestReadDoesNotAdvanceRoundRobin(t *testing.T) {
	good := provider{list: func(context.Context, compute.ListRequest) (compute.ListResponse, error) {
		return compute.ListResponse{Items: []compute.Instance{}}, nil
	}}
	a := newAPI(t, map[string]compute.Provider{"b": good, "a": good})
	services := []scheduler.Service{{Name: "b", Priority: 1, Provider: good}, {Name: "a", Priority: 1, Provider: good}}
	if err := a.Controller.Scheduler.Configure("scope", services); err != nil {
		t.Fatal(err)
	}
	h := a.Handler()
	first := get(h, "/scheduler/round-robin").Body.String()
	for range 5 {
		if next := get(h, "/scheduler/round-robin").Body.String(); next != first {
			t.Fatal("read advanced cursor")
		}
	}
	if !strings.Contains(first, `"next_service":"a"`) {
		t.Fatal(first)
	}
}
func TestAggregateDeadlineAndPaginationCycle(t *testing.T) {
	p := provider{list: func(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
		return compute.ListResponse{Items: []compute.Instance{}, NextPageToken: "same"}, nil
	}}
	a := newAPI(t, map[string]compute.Provider{"p": p})
	if w := get(a.Handler(), "/instances"); w.Code != 502 {
		t.Fatal(w.Code)
	}
	p.list = func(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
		<-ctx.Done()
		return compute.ListResponse{}, ctx.Err()
	}
	a = newAPI(t, map[string]compute.Provider{"p": p})
	a.Controller.Config.Call = 10 * time.Millisecond
	if w := get(a.Handler(), "/instances"); w.Code != 502 {
		t.Fatal(w.Code)
	}
}
