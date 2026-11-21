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

	compute "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/controller"
	"github.com/colony-2/pulse/internal/providers/adapter"
	"github.com/colony-2/pulse/internal/providers/remote"
	"github.com/colony-2/pulse/internal/scheduler"
)

type fakeProvider struct {
	list func(context.Context, compute.ListRequest) (compute.ListResponse, error)
}

func (p fakeProvider) Submit(context.Context, []compute.Launch, compute.SubmitOptions) ([]compute.Submission, compute.LogSubscription, error) {
	panic("read-only endpoint submitted work")
}
func (p fakeProvider) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	return p.list(ctx, q)
}
func row(id string) compute.Instance {
	return compute.Instance{ID: id, LaunchID: id, State: "running", Job: compute.JobRef{JobDB: "https://db/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}, Refs: []string{id}}
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
		// Unknown fakeProvider is checked before its query parameters.
		w := get(h, path)
		if w.Code != 400 && w.Code != 404 {
			t.Fatal(w)
		}
	}
	if w := get(h, "/config"); !strings.Contains(w.Body.String(), "[REDACTED]") {
		t.Fatal(w.Body.String())
	}
}

func TestRemoteErrorBodyStaysOutOfPublicResponses(t *testing.T) {
	const raw = "operator-only-native-quota-error"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(raw))
	}))
	defer s.Close()
	native, err := remote.New(s.URL, s.Client(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(t, map[string]compute.Provider{"remote": &adapter.Provider{Native: native}})
	for _, path := range []string{"/instances", "/providers/remote/instances"} {
		w := get(a.Handler(), path)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), raw) || !json.Valid(w.Body.Bytes()) {
			t.Fatal("raw error entered public response", path, w.Code, w.Body.String())
		}
	}
}
func TestAggregatePagesPartialFailureAndProviderSelection(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	good := fakeProvider{list: func(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
		mu.Lock()
		calls = append(calls, q.PageToken)
		mu.Unlock()
		if q.PageToken == "next" {
			return compute.ListResponse{Items: []compute.Instance{row("b")}}, nil
		}
		return compute.ListResponse{Items: []compute.Instance{row("a")}, NextPageToken: "next"}, nil
	}}
	bad := fakeProvider{list: func(context.Context, compute.ListRequest) (compute.ListResponse, error) {
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
	good := fakeProvider{list: func(context.Context, compute.ListRequest) (compute.ListResponse, error) {
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
	p := fakeProvider{list: func(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
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

func (fakeProvider) GetCapabilities(context.Context) (compute.Capabilities, error) {
	return compute.Capabilities{}, nil
}

type placementProvider struct {
	fakeProvider
	submissions int
}

func (p *placementProvider) Submit(_ context.Context, ls []compute.Launch, _ compute.SubmitOptions) ([]compute.Submission, compute.LogSubscription, error) {
	p.submissions++
	return []compute.Submission{{LaunchID: ls[0].LaunchID, Status: compute.NoCapacity, Reason: "operator-only-reason-sentinel"}}, nil, errors.New("operator-only-error-sentinel")
}
func TestPlacementErrorsStayInOperatorDiagnostics(t *testing.T) {
	lists := 0
	p := &placementProvider{fakeProvider: fakeProvider{list: func(context.Context, compute.ListRequest) (compute.ListResponse, error) {
		lists++
		return compute.ListResponse{Items: []compute.Instance{}}, nil
	}}}
	a := newAPI(t, map[string]compute.Provider{"p": p})
	out, err := a.Controller.Scheduler.Run(t.Context(), "scope", []scheduler.Service{{Name: "p", Priority: 1, Provider: p}}, []scheduler.Job{{
		Key:     scheduler.Key{Instance: "db", Tenant: "tenant", Job: "job"},
		Request: compute.Launch{LaunchID: "launch", Allocation: compute.Allocation{Base: "ubuntu-26*"}, Job: compute.JobRef{JobDB: "https://db/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}, Lease: "lease-sentinel"},
	}})
	if err != nil || len(out) != 1 || p.submissions != 1 || lists != 0 || out[0].Attempts[0].SubmissionError == nil || *out[0].Attempts[0].SubmissionError != "operator-only-error-sentinel" {
		t.Fatal(out, err, lists)
	}
	for _, path := range []string{"/", "/status", "/config", "/instances", "/providers/p/instances", "/scheduler/cooldowns", "/scheduler/round-robin"} {
		response := get(a.Handler(), path)
		if response.Code != 200 || !json.Valid(response.Body.Bytes()) {
			t.Fatal(path, response.Code, response.Body.String())
		}
		for _, sentinel := range []string{"operator-only-reason-sentinel", "operator-only-error-sentinel", "lease-sentinel", "requested_jobdb_until", "execution_admission"} {
			if strings.Contains(response.Body.String(), sentinel) {
				t.Fatal("operator diagnostics leaked to public response", path, response.Body.String())
			}
		}
	}
	if lists != 2 || p.submissions != 1 {
		t.Fatal("unexpected provider calls", lists, p.submissions)
	}
}
