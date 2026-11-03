package providerplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/providers/adapter"
	"github.com/colony-2/pulse/internal/scheduler"
	"github.com/colony-2/pulse/pkg/compute"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fixtureConfig struct {
	Mode         string `yaml:"mode"`
	Journal      string `yaml:"journal"`
	RequiredFile string `yaml:"required_file"`
	Value        string `yaml:"value"`
}
type fixtureProvider struct {
	cfg   fixtureConfig
	life  context.Context
	mu    sync.Mutex
	items []compute.Instance
}

func event(path string, v any) {
	b, _ := json.Marshal(v)
	f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		os.Exit(41)
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Sync()
	_ = f.Close()
}
func TestPluginHelperProcess(t *testing.T) {
	if os.Getenv("PULSE_PLUGIN_COOKIE") == "" {
		return
	}
	var cfg fixtureConfig
	Serve(func(ctx context.Context, i Init) (compute.Provider, error) {
		if DecodeConfig(i.ConfigYAML, &cfg) != nil || cfg.Journal == "" {
			return nil, errors.New("CANARY bad config")
		}
		if cfg.RequiredFile != "" {
			path, e := ResolvePath(i.BaseDir, cfg.RequiredFile)
			if e != nil {
				return nil, e
			}
			if _, e = os.ReadFile(path); e != nil {
				return nil, e
			}
		}
		cwd, _ := os.Getwd()
		event(cfg.Journal, map[string]any{"kind": "configure", "pid": os.Getpid(), "value": cfg.Value, "cwd": cwd, "base": i.BaseDir, "selected": os.Getenv("SELECTED_CANARY"), "unselected": os.Getenv("PLUGIN_TEST_UNSELECTED")})
		if cfg.Mode == "init_block" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if cfg.Mode == "init_fail" {
			return nil, errors.New("CANARY")
		}
		if cfg.Mode == "auto_exit" {
			go func() { time.Sleep(150 * time.Millisecond); os.Exit(42) }()
		}
		return &fixtureProvider{cfg: cfg, life: i.Lifetime}, nil
	}, ServeOptions{Implementation: "fixture", Version: "1", MaxMessageBytes: 2 << 20, Features: []string{"fixture"}, Shutdown: func(ctx context.Context) error {
		if cfg.Mode == "shutdown_block" {
			<-ctx.Done()
		}
		event(cfg.Journal, map[string]any{"kind": "shutdown"})
		return nil
	}})
	os.Exit(0)
}
func (f *fixtureProvider) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	event(f.cfg.Journal, map[string]any{"kind": "submit", "launch": ls[0].LaunchID})
	if f.cfg.Mode == "crash" {
		os.Exit(43)
	}
	if f.cfg.Mode == "block" || f.cfg.Mode == "cancel" {
		<-ctx.Done()
		event(f.cfg.Journal, map[string]any{"kind": "canceled"})
		return nil, ctx.Err()
	}
	select {
	case <-f.life.Done():
		return nil, errors.New("lifetime canceled")
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range ls {
		f.items = append(f.items, compute.Instance{ID: l.LaunchID, LaunchID: l.LaunchID, State: "running", Metadata: l.Metadata, Refs: []string{}})
	}
	return results(ls, compute.Accepted, ""), nil
}
func (f *fixtureProvider) List(_ context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return compute.Page(append([]compute.Instance{}, f.items...), q)
}
func fixtureOptions(t *testing.T, mode string) ProcessConfig {
	t.Helper()
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	return ProcessConfig{Command: binary, Args: []string{"-test.run=^TestPluginHelperProcess$"}, Name: "fixture", ConfigYAML: []byte(fmt.Sprintf("mode: %s\njournal: %q\nvalue: original\n", mode, filepath.Join(dir, "events"))), BaseDir: dir, StartTimeout: 15 * time.Second, ShutdownTimeout: 200 * time.Millisecond}
}
func startFixture(t *testing.T, c ProcessConfig) *Process {
	t.Helper()
	p, e := Start(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(p.Close)
	return p
}
func events(t *testing.T, c ProcessConfig) []map[string]any {
	t.Helper()
	var cfg fixtureConfig
	_ = DecodeConfig(c.ConfigYAML, &cfg)
	b, _ := os.ReadFile(cfg.Journal)
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func countEvents(t *testing.T, c ProcessConfig, kind string) int {
	n := 0
	for _, e := range events(t, c) {
		if e["kind"] == kind {
			n++
		}
	}
	return n
}
func TestProcessLifetimeIsolationConcurrency(t *testing.T) {
	c := fixtureOptions(t, "accept")
	c.PassEnv = []string{"PLUGIN_TEST_SELECTED"} // These names are intentionally reserved.
	if _, e := Start(context.Background(), c); e == nil {
		t.Fatal("accepted reserved environment")
	}
	c.PassEnv = []string{"SELECTED_CANARY"}
	t.Setenv("SELECTED_CANARY", "secret")
	t.Setenv("PLUGIN_TEST_UNSELECTED", "hidden")
	ctx, cancel := context.WithCancel(context.Background())
	p, e := Start(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ls := []compute.Launch{launch(fmt.Sprintf("l%d", i))}
			out, e := p.Submit(context.Background(), ls)
			if e != nil && out[0].Status != compute.Unavailable {
				t.Error(e)
			}
			_, _ = p.List(context.Background(), compute.ListRequest{})
		}(i)
	}
	wg.Wait()
	out, e := p.Submit(context.Background(), []compute.Launch{launch("final")})
	if e != nil || out[0].Status != compute.Accepted {
		t.Fatal(out, e)
	}
	entries := events(t, c)
	if entries[0]["cwd"] == c.BaseDir || entries[0]["base"] != c.BaseDir || entries[0]["unselected"] != "" || entries[0]["selected"] != "secret" {
		t.Fatal(entries[0])
	}
	p.Close()
	p.Close()
	if _, e = p.List(context.Background(), compute.ListRequest{}); e == nil {
		t.Fatal("list after close succeeded")
	}
}
func TestCrashNoFallbackNoReleaseNoReplay(t *testing.T) {
	c := fixtureOptions(t, "crash")
	p := startFixture(t, c)
	fallback := &countProvider{}
	released := false
	l := provider.Launch{LaunchID: "original", Allocation: provider.Allocation{Base: "ubuntu-26.04-arm64"}, Job: provider.JobRef{JobDB: "https://db/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}, Lease: "test-lease"}
	s := scheduler.New(time.Minute, time.Second)
	job := scheduler.Job{Key: scheduler.Key{Instance: "db", Tenant: "tenant", Job: "job"}, Request: l, Prepare: func(_ context.Context, r provider.Launch) (provider.Launch, func(context.Context) error, error) {
		l.LaunchID = r.LaunchID
		return l, func(context.Context) error { released = true; return nil }, nil
	}}
	out, e := s.Run(context.Background(), "scope", []scheduler.Service{{Name: "plugin", Priority: 1, Provider: &adapter.Provider{Native: p}}, {Name: "fallback", Priority: 2, Provider: &adapter.Provider{Native: fallback}}}, []scheduler.Job{job})
	if e != nil || len(out) != 1 || out[0].Submission.Status != provider.Unknown || fallback.calls != 0 || released {
		t.Fatal(out, e, fallback.calls, released)
	}
	waitFor(t, func() bool { return p.Health().Generation == 2 && p.Health().State == "ready" })
	if countEvents(t, c, "submit") != 1 {
		t.Fatal("replayed a launch")
	}
	if _, e = p.List(context.Background(), compute.ListRequest{}); e != nil {
		t.Fatal(e)
	}
}

type countProvider struct{ calls int }

func (f *countProvider) Submit(_ context.Context, l []compute.Launch) ([]compute.Submission, error) {
	f.calls++
	return results(l, compute.Accepted, ""), nil
}
func (f *countProvider) List(context.Context, compute.ListRequest) (compute.ListResponse, error) {
	return compute.ListResponse{Items: []compute.Instance{}}, nil
}
func TestCancellationOverloadAndLimits(t *testing.T) {
	c := fixtureOptions(t, "block")
	c.MaxConcurrent = 1
	p := startFixture(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []compute.Submission, 1)
	go func() { out, _ := p.Submit(ctx, []compute.Launch{launch("first")}); done <- out }()
	waitFor(t, func() bool { return countEvents(t, c, "submit") == 1 })
	out, _ := p.Submit(context.Background(), []compute.Launch{launch("second")})
	if out[0].Status != compute.Unavailable {
		t.Fatal(out)
	}
	if _, e := p.List(context.Background(), compute.ListRequest{}); e == nil {
		t.Fatal("list overload succeeded")
	}
	cancel()
	out = <-done
	if out[0].Status != compute.Unknown {
		t.Fatal(out)
	}
	waitFor(t, func() bool { return countEvents(t, c, "canceled") == 1 })
	if p.Health().Generation != 1 || p.Health().State != "ready" {
		t.Fatal(p.Health())
	}
	ls := []compute.Launch{launch("large1"), launch("large2")}
	for i := range ls {
		ls[i].Process.Stdin = compute.SecretInput(strings.Repeat("x", compute.MaxStdin))
	}
	out, _ = p.Submit(context.Background(), ls)
	if out[0].Status != compute.Rejected || countEvents(t, c, "submit") != 1 {
		t.Fatal("oversized batch dispatched")
	}
}
func TestStartupFailuresChecksumFeaturesAndPaths(t *testing.T) {
	c := fixtureOptions(t, "accept")
	c.ConfigYAML = append(c.ConfigYAML, []byte("required_file: settings.txt\n")...)
	if _, e := Start(context.Background(), c); e == nil {
		t.Fatal("missing file accepted")
	}
	if e := os.WriteFile(filepath.Join(c.BaseDir, "settings.txt"), []byte("ok"), 0600); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(c.Command)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(data)
	c.SHA256 = hex.EncodeToString(sum[:])
	p := startFixture(t, c)
	p.Close()
	c.SHA256 = strings.Repeat("0", 64)
	if _, e := Start(context.Background(), c); e == nil {
		t.Fatal("checksum mismatch accepted")
	}
	c.SHA256 = ""
	c.RequiredFeatures = []string{"unsupported"}
	if _, e := Start(context.Background(), c); e == nil {
		t.Fatal("missing feature accepted")
	}
	c = fixtureOptions(t, "init_block")
	c.StartTimeout = 150 * time.Millisecond
	before := time.Now()
	if _, e := Start(context.Background(), c); e == nil {
		t.Fatal("startup timeout accepted")
	}
	if time.Since(before) > 2*time.Second {
		t.Fatal("unbounded startup")
	}
	c = fixtureOptions(t, "accept")
	c.Command = "relative"
	if _, e := Start(context.Background(), c); e == nil {
		t.Fatal("relative command accepted")
	}
}
func TestRestartSnapshotAndBudget(t *testing.T) {
	c := fixtureOptions(t, "auto_exit")
	p := startFixture(t, c)
	copy(c.ConfigYAML, []byte(strings.Repeat("x", len(c.ConfigYAML))))
	waitFor(t, func() bool { return p.Health().State == "failed" })
	h := p.Health()
	if h.Restarts != 3 || h.Generation != 4 {
		t.Fatal(h)
	}
	// The process owns a deep copy; inspect the original journal through its copy.
	es := events(t, p.cfg)
	for _, e := range es {
		if e["kind"] == "configure" && e["value"] != "original" {
			t.Fatal("configuration snapshot changed")
		}
	}
	out, _ := p.Submit(context.Background(), []compute.Launch{launch("after")})
	if out[0].Status != compute.Unavailable {
		t.Fatal(out)
	}
}

func TestDeadlineDoesNotKillConcurrentList(t *testing.T) {
	c := fixtureOptions(t, "block")
	p := startFixture(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan []compute.Submission, 1)
	go func() { r, _ := p.Submit(ctx, []compute.Launch{launch("deadline")}); done <- r }()
	waitFor(t, func() bool { return countEvents(t, c, "submit") == 1 })
	if _, e := p.List(context.Background(), compute.ListRequest{}); e != nil {
		t.Fatal(e)
	}
	if out := <-done; out[0].Status != compute.Unknown {
		t.Fatal(out)
	}
	waitFor(t, func() bool { return countEvents(t, c, "canceled") == 1 })
	if p.Health().Generation != 1 {
		t.Fatal("deadline restarted provider")
	}
}
func TestCloseWithHungShutdown(t *testing.T) {
	c := fixtureOptions(t, "shutdown_block")
	p := startFixture(t, c)
	before := time.Now()
	p.Close()
	if time.Since(before) > 3*time.Second {
		t.Fatal("unbounded shutdown")
	}
	es := events(t, c)
	pid := int(es[0]["pid"].(float64))
	if processAlive(pid) {
		t.Fatal("owned process survived shutdown")
	}
}
func TestPluginHostHelperProcess(t *testing.T) {
	journal := os.Getenv("PULSE_TEST_HOST_JOURNAL")
	if journal == "" {
		return
	}
	binary, _ := os.Executable()
	_, e := Start(context.Background(), ProcessConfig{Command: binary, Args: []string{"-test.run=^TestPluginHelperProcess$"}, BaseDir: filepath.Dir(journal), ConfigYAML: []byte(fmt.Sprintf("mode: accept\njournal: %q\n", journal))})
	if e != nil {
		os.Exit(45)
	}
	// Abrupt host loss must close the inherited parent pipe without Close.
	os.Exit(44)
}
func TestAbruptHostLoss(t *testing.T) {
	c := fixtureOptions(t, "accept")
	var cfg fixtureConfig
	_ = DecodeConfig(c.ConfigYAML, &cfg)
	cmd := exec.Command(c.Command, "-test.run=^TestPluginHostHelperProcess$")
	cmd.Env = append(os.Environ(), "PULSE_TEST_HOST_JOURNAL="+cfg.Journal)
	if e := cmd.Run(); e == nil {
		t.Fatal("host unexpectedly exited cleanly")
	}
	es := events(t, c)
	if len(es) == 0 {
		t.Fatal("plugin was not initialized")
	}
	pid := int(es[0]["pid"].(float64))
	t.Cleanup(func() {
		if processAlive(pid) {
			p, _ := os.FindProcess(pid)
			_ = p.Kill()
		}
	})
	waitFor(t, func() bool { return !processAlive(pid) })
	if countEvents(t, c, "shutdown") != 1 {
		t.Fatal("host-loss shutdown hook did not run")
	}
}
func processAlive(pid int) bool {
	// Linux containers may retain orphan zombies until their init reaps them.
	if b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); e == nil {
		parts := strings.Fields(string(b))
		if len(parts) > 2 && parts[2] == "Z" {
			return false
		}
	}
	return syscall.Kill(pid, 0) == nil
}
