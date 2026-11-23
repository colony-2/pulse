package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/controller"
	"github.com/colony-2/pulse/internal/httpapi"
	"github.com/colony-2/pulse/internal/launchlogs"
	"github.com/colony-2/pulse/internal/providers"
	"github.com/colony-2/pulse/internal/scheduler"
)

type reverseLogFixture struct {
	emit <-chan struct{}
	data []byte
}

func (p reverseLogFixture) GetCapabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-arm64"}}}, nil
}

func (p reverseLogFixture) List(context.Context, api.ListRequest) (api.ListResponse, error) {
	return api.ListResponse{Items: []api.Instance{}}, nil
}

func (p reverseLogFixture) GetLaunchLog(_ context.Context, q api.LaunchLogRequest) (api.LaunchLogResponse, error) {
	if q.LaunchID != "retained" {
		return api.LaunchLogResponse{}, api.ErrLaunchLogUnsupported
	}
	const checkpoint = "\x00\xff"
	size := int64(len(p.data))
	page := api.LaunchLogResponse{Records: []api.LaunchLogRecord{}, NextPageToken: checkpoint, Finalized: true, EstimatedTotalSizeBytes: &size}
	switch q.PageToken {
	case "":
		page.Records = append(page.Records, api.LaunchLogRecord{Sequence: 1, Timestamp: time.Now(), Source: api.LogSourceStdout, Data: p.data})
	case checkpoint:
	default:
		return api.LaunchLogResponse{}, api.ErrLaunchLogCursor
	}
	return page, nil
}

func (p reverseLogFixture) Submit(_ context.Context, launches []api.Launch, options api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	id := launches[0].LaunchID
	out := []api.Submission{{LaunchID: id, Status: api.Accepted}}
	if options.Listener == nil {
		return out, nil, nil
	}
	ctx, stop := context.WithCancel(options.Listener.Context)
	sub := &reverseLogSubscription{id: id, done: make(chan struct{}), stop: stop}
	go func() {
		defer close(sub.done)
		select {
		case <-ctx.Done():
			sub.err = ctx.Err()
		case <-p.emit:
			sub.err = options.Listener.OnLog(ctx, api.LaunchLog{LaunchID: id, SubscriptionID: "fixture", Sequence: 1,
				Timestamp: time.Now(), Source: api.LogSourceStdout, Data: p.data})
		}
	}()
	return out, sub, nil
}

type reverseLogSubscription struct {
	id   string
	done chan struct{}
	stop context.CancelFunc
	err  error
}

func (s *reverseLogSubscription) Stop()                 { s.stop() }
func (s *reverseLogSubscription) Done() <-chan struct{} { return s.done }
func (s *reverseLogSubscription) Coverage() []api.SourceStatus {
	state, closed, sequence := api.SourceActive, false, uint64(0)
	select {
	case <-s.done:
		if s.err == nil {
			state, closed, sequence = api.SourceClosed, true, 1
		} else {
			state = api.SourceInterrupted
		}
	default:
	}
	return []api.SourceStatus{
		{LaunchID: s.id, Source: api.LogSourceLaunch, State: state, Closed: closed},
		{LaunchID: s.id, Source: api.LogSourceStdout, State: state, Closed: closed, LastSequence: sequence},
		{LaunchID: s.id, Source: api.LogSourceStderr, State: state, Closed: closed},
	}
}
func (s *reverseLogSubscription) Wait(ctx context.Context) (api.ListenerResult, error) {
	select {
	case <-ctx.Done():
		return api.ListenerResult{}, ctx.Err()
	case <-s.done:
		reason := api.ListenerSourcesClosed
		if s.err != nil {
			reason = api.ListenerSourceError
		}
		return api.ListenerResult{Reason: reason, Cause: s.err, Coverage: s.Coverage()}, s.err
	}
}

func TestReverseProviderLogsThroughREST(t *testing.T) {
	const key = "reverse-fixture-shared-key"
	t.Setenv("PULSE_REVERSE_LOGS_TEST_KEY", key)
	cfg, err := config.Parse([]byte(`
reverse:
  listen: 127.0.0.1:0
  transport: http
providers:
  worker: {type: reverse, token_env: PULSE_REVERSE_LOGS_TEST_KEY}
targets:
  - jobdb: https://db.example/tenant
    launch_services: [{name: worker, priority: 1}]
`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	server, err := providers.StartReverse(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	providerCtx, stopProvider := context.WithCancel(ctx)
	done := make(chan error, 1)
	defer func() {
		stopProvider()
		if err := <-done; err != nil && !errors.Is(err, ctx.Err()) && !errors.Is(err, context.Canceled) {
			t.Errorf("reverse provider: %v", err)
		}
	}()
	emit := make(chan struct{})
	data := []byte{0xff, 0, 'a', '\n'}
	go func() {
		done <- sdk.Run(providerCtx, func(context.Context, sdk.Init) (api.Provider, sdk.Cleanup, error) {
			return reverseLogFixture{emit: emit, data: data}, nil, nil
		}, sdk.RunOptions{
			Address: server.Address, AuthMode: sdk.ReverseAuthMutualKey, Transport: sdk.ReverseTransportHTTP,
			MutualKey: []byte(key), Init: sdk.Initialization{Name: "worker", BaseDir: cfg.BaseDir},
			Serve: sdk.ServeOptions{Implementation: "test-logs", Version: "test"},
		})
	}()
	for {
		if _, err := server.Providers["worker"].GetCapabilities(ctx); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("reverse provider did not become available")
		case <-time.After(10 * time.Millisecond):
		}
	}
	store, err := launchlogs.Open(ctx, t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sched := scheduler.New(time.Minute, time.Second)
	sched.Logs = store
	httpAPI, err := httpapi.New(&controller.Controller{Config: cfg, Providers: server.Providers, Scheduler: sched}, "test")
	if err != nil {
		t.Fatal(err)
	}
	handler := httpAPI.Handler()
	readHealth := func() sdk.Health {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/providers/health", nil))
		var health struct{ Items []sdk.Health }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &health) != nil || len(health.Items) != 1 {
			t.Fatalf("reverse health HTTP %d: %s", w.Code, w.Body)
		}
		return health.Items[0]
	}
	if health := readHealth(); health.Name != "worker" || health.State != "ready" || health.Implementation != "test-logs" || health.Generation == 0 {
		t.Fatalf("unexpected connected provider health: %+v", health)
	}
	read := func(id, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/providers/worker/launches/"+id+"/logs?page_size=1", nil)
		if token != "" {
			q := r.URL.Query()
			q.Set("page_token", token)
			r.URL.RawQuery = q.Encode()
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	parse := func(w *httptest.ResponseRecorder) launchlogs.Page {
		t.Helper()
		var page launchlogs.Page
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatalf("log response %d: %s", w.Code, w.Body)
		}
		return page
	}
	native := parse(read("retained", ""))
	if len(native.Records) != 1 || !bytes.Equal(native.Records[0].Data, data) || !native.Finalized || native.EstimatedTotalSizeBytes == nil || *native.EstimatedTotalSizeBytes != int64(len(data)) {
		t.Fatal(native)
	}
	if end := parse(read("retained", native.NextPageToken)); len(end.Records) != 0 || end.NextPageToken != native.NextPageToken {
		t.Fatal(end)
	}

	job := scheduler.Job{
		Key: scheduler.Key{Instance: "db", Tenant: "tenant", Job: "job"},
		Request: api.Launch{LaunchID: "live", Allocation: api.Allocation{Base: "ubuntu*"}, Lease: "fixture-lease",
			Job: api.JobRef{JobDB: "https://db.example/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}},
	}
	results, err := sched.Run(ctx, "test", []scheduler.Service{{Name: "worker", Priority: 1, Provider: server.Providers["worker"]}}, []scheduler.Job{job})
	if err != nil || len(results) != 1 || results[0].Submission.Status != api.Accepted {
		t.Fatal(results, err)
	}
	close(emit)
	var live launchlogs.Page
	for {
		w := read("live", "")
		if w.Code == http.StatusOK {
			live = parse(w)
		} else if w.Code != http.StatusNotFound {
			t.Fatalf("unexpected live log response %d: %s", w.Code, w.Body)
		}
		if live.Finalized {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("listener did not persist logs after admission: %+v", live)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if len(live.Records) != 1 || !bytes.Equal(live.Records[0].Data, data) || live.Incomplete {
		t.Fatal(live)
	}
	if end := parse(read("live", live.NextPageToken)); len(end.Records) != 0 || end.NextPageToken != live.NextPageToken {
		t.Fatal("file cursor did not retain its position", end)
	}
	stopProvider()
	for {
		health := readHealth()
		if health.Name == "worker" && health.State == "unavailable" && health.Generation == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("reverse provider health did not reflect disconnect: %+v", health)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
