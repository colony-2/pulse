package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	docker "github.com/colony-2/pulse-docker"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
)

func dockerConfig(t *testing.T, socket string) *config.Config {
	t.Helper()
	if strings.HasPrefix(socket, "/") {
		socket = "unix://" + socket
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`providers:
  docker: {type: docker, socket: %q}
targets:
- instance_id: original
  jobdb: http://localhost/tenant
  launch_services: [{name: docker, priority: 1}]
`, socket)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDockerSettings(t *testing.T) {
	t.Setenv("PULSE_TEST_AUTH", "synthetic-private-auth")
	cfg := dockerConfig(t, "/tmp/test-daemon.sock")
	cfg.Defaults.Env = map[string]string{"CUSTOM": "value"}
	p := cfg.Providers["docker"]
	p.RegistryAuthEnv = "PULSE_TEST_AUTH"
	c, err := dockerSettings(p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.Overhead != 256<<20 || c.CPUMillis != 0 || c.MemoryBytes != 0 || c.MaxContainers != 0 || c.ScratchPath != "/scratch" || c.LockDir != filepath.Join(os.TempDir(), "pulse-docker-locks") || c.DependencyCache.Namespace != "default" || c.DependencyCache.Generation != "1" || c.DependencyCache.MaxAge != 720*time.Hour || c.DependencyCache.Disabled || c.RegistryAuth != "synthetic-private-auth" {
		t.Fatal("host defaults changed")
	}
	if len(c.LegacyEndpoints) != 1 || c.LegacyEndpoints[0].InstanceID != "original" || c.LegacyEndpoints[0].TenantID != "tenant" || c.LegacyEndpoints[0].Endpoint != "http://localhost/tenant" {
		t.Fatal("legacy identity changed")
	}
	cfg.Defaults.Env["CUSTOM"] = "mutated"
	cfg.Targets[0].JobDB = "http://other/tenant"
	if c.ExecutorEnv["CUSTOM"] != "value" || c.LegacyEndpoints[0].Endpoint != "http://localhost/tenant" {
		t.Fatal("settings retain caller data")
	}

	p.Overhead, p.Capacity.CPU, p.Capacity.Memory = "0", "1.5", "1Gi"
	p.ScratchPath, p.LockDir = "/tmp/scratch", "relative-locks"
	p.RegistryAuthEnv = "PULSE_TEST_UNSET_AUTH"
	t.Setenv(p.RegistryAuthEnv, "")
	p.Capacity.MaxContainers = 3
	p.DependencyCache = &config.DependencyCache{Namespace: "cache", Generation: "2", MaxAge: "24h"}
	c, err = dockerSettings(p, cfg)
	if err != nil || c.Overhead != 0 || c.CPUMillis != 1500 || c.MemoryBytes != 1<<30 || c.MaxContainers != 3 || c.RegistryAuth != "" || c.LockDir != "relative-locks" || c.ScratchPath != "/tmp/scratch" || c.DependencyCache.MaxAge != 24*time.Hour {
		t.Fatal("overrides changed", err)
	}
	q := p
	q.Capacity.CPU, q.Capacity.Memory = "1500m", "1024Mi"
	q.DependencyCache = &config.DependencyCache{Namespace: "cache", Generation: "2", MaxAge: "1440m"}
	equivalent, err := dockerSettings(q, cfg)
	if err != nil || !sameDockerSettings(c, equivalent) {
		t.Fatal("equivalent quantities differ", err)
	}

	for _, field := range []string{"cpu", "memory", "overhead", "duration", "endpoint", "conflict"} {
		t.Run(field, func(t *testing.T) {
			cfg := dockerConfig(t, "/tmp/test-daemon.sock")
			p := cfg.Providers["docker"]
			switch field {
			case "cpu":
				p.Capacity.CPU = "invalid"
			case "memory":
				p.Capacity.Memory = "invalid"
			case "overhead":
				p.Overhead = "invalid"
			case "duration":
				p.DependencyCache = &config.DependencyCache{MaxAge: "synthetic-private-auth"}
			case "endpoint":
				cfg.Targets[0].JobDB = "http://user:synthetic-private-auth@localhost/tenant"
			case "conflict":
				target := cfg.Targets[0]
				target.JobDB = "http://other/tenant"
				cfg.Targets = append(cfg.Targets, target)
			}
			_, err := dockerSettings(p, cfg)
			if err == nil || strings.Contains(err.Error(), "synthetic-private-auth") {
				t.Fatal("invalid settings accepted or secret leaked")
			}
		})
	}
}

func TestDockerAliasComparison(t *testing.T) {
	cfg := dockerConfig(t, "/tmp/test-daemon.sock")
	p := cfg.Providers["docker"]
	base, err := dockerSettings(p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	explicit := p
	explicit.LockDir, explicit.ScratchPath = base.LockDir, base.ScratchPath
	explicit.Overhead = "256Mi"
	explicit.DependencyCache = &config.DependencyCache{Namespace: "default", Generation: "1", MaxAge: "720h"}
	cfg.Targets = append(cfg.Targets, cfg.Targets[0])
	equivalent, err := dockerSettings(explicit, cfg)
	if err != nil || !sameDockerSettings(base, equivalent) {
		t.Fatal("normalized defaults or duplicate endpoint differ", err)
	}
	base.ExecutorEnv = nil
	equivalent.ExecutorEnv = map[string]string{}
	if !sameDockerSettings(base, equivalent) {
		t.Fatal("nil/empty env differ")
	}
	for name, change := range map[string]func(*docker.Config){
		"socket":     func(c *docker.Config) { c.Socket += "2" },
		"lock":       func(c *docker.Config) { c.LockDir += "2" },
		"scratch":    func(c *docker.Config) { c.ScratchPath += "2" },
		"auth":       func(c *docker.Config) { c.RegistryAuth = "private" },
		"cpu":        func(c *docker.Config) { c.CPUMillis++ },
		"memory":     func(c *docker.Config) { c.MemoryBytes++ },
		"overhead":   func(c *docker.Config) { c.Overhead++ },
		"slots":      func(c *docker.Config) { c.MaxContainers++ },
		"disabled":   func(c *docker.Config) { c.DependencyCache.Disabled = true },
		"namespace":  func(c *docker.Config) { c.DependencyCache.Namespace += "2" },
		"generation": func(c *docker.Config) { c.DependencyCache.Generation += "2" },
		"age":        func(c *docker.Config) { c.DependencyCache.MaxAge++ },
		"env":        func(c *docker.Config) { c.ExecutorEnv = map[string]string{"CUSTOM": "different"} },
		"endpoint": func(c *docker.Config) {
			c.LegacyEndpoints = []docker.LegacyEndpoint{{InstanceID: "another", TenantID: "tenant", Endpoint: "http://localhost/tenant"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			altered := base
			change(&altered)
			if sameDockerSettings(base, altered) {
				t.Fatal("conflict ignored")
			}
		})
	}
}

// This is only a construction/listing double. Backend admission, caches and
// execution are covered by pulse-docker and by the separate live Pulse suite.
func dockerDaemon(t *testing.T, observe ...func(http.ResponseWriter, *http.Request) bool) (string, *atomic.Int32) {
	t.Helper()
	var opens atomic.Int32
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.47")
		for _, f := range observe {
			if f(w, r) {
				return
			}
		}
		switch path {
		case "/version":
			opens.Add(1)
			json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.47"})
		case "/info":
			json.NewEncoder(w).Encode(map[string]any{"ID": "pulse-test-daemon", "NCPU": 8, "MemTotal": int64(16 << 30), "OSType": "linux", "Architecture": "arm64"})
		case "/containers/json":
			fmt.Fprint(w, `[]`)
		case "/volumes":
			fmt.Fprint(w, `{"Volumes":[]}`)
		default:
			t.Errorf("unexpected daemon call %s %s", r.Method, path)
			http.Error(w, "unexpected", 500)
		}
	}))
	s.Listener = listener
	s.Start()
	t.Cleanup(s.Close)
	return socket, &opens
}

func TestDockerDirectProviderAliasesAndCleanup(t *testing.T) {
	socket, opens := dockerDaemon(t)
	cfg := dockerConfig(t, socket)
	p := cfg.Providers["docker"]
	p.LockDir = t.TempDir()
	cfg.Providers["docker"] = p
	alias := p
	alias.ScratchPath, alias.Overhead = "/scratch", "256Mi"
	alias.DependencyCache = &config.DependencyCache{Namespace: "default", Generation: "1", MaxAge: "720h"}
	link := filepath.Join(t.TempDir(), "alias.sock")
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	alias.Socket = "unix://" + link
	cfg.Providers["alias"] = alias
	ctx, cancel := context.WithTimeout(t.Context(), time.Second*5)
	defer cancel()
	instances, closeAll, err := Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeAll)
	public, ok := instances["docker"].(*docker.Provider)
	if !ok || instances["alias"] != public || opens.Load() != 1 {
		t.Fatal("direct provider or shared ownership lost")
	}
	if _, ok := instances["docker"].(interface{ Maintain(context.Context) error }); !ok {
		t.Fatal("maintenance hidden")
	}
	caps, err := public.GetCapabilities(ctx)
	if err != nil || caps.BasesComplete || len(caps.Bases) == 0 {
		t.Fatal("public capabilities", err)
	}
	page, err := public.List(ctx, api.ListRequest{})
	if err != nil || page.Items == nil || len(page.Items) != 0 {
		t.Fatal("public empty list", err)
	}
	if _, err := public.List(ctx, api.ListRequest{PageToken: "invalid"}); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	closeAll()
	closeAll()
	if _, err := public.GetCapabilities(ctx); err == nil {
		t.Fatal("cleanup did not close provider")
	}
	_, closeAgain, err := Build(ctx, cfg)
	if err != nil {
		t.Fatal("ownership not released", err)
	}
	closeAgain()

	alias.Overhead = "0"
	cfg.Providers["alias"] = alias
	before := opens.Load()
	if _, _, err = Build(ctx, cfg); err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Fatal("conflict not rejected", err)
	}
	if opens.Load() != before+1 {
		t.Fatal("conflict constructed another owner")
	}
	delete(cfg.Providers, "alias")
	_, closeFinal, err := Build(ctx, cfg)
	if err != nil {
		t.Fatal("failure cleanup retained ownership", err)
	}
	closeFinal()
}

func TestDockerCapabilityDiagnostics(t *testing.T) {
	socket, _ := dockerDaemon(t)
	cfg := dockerConfig(t, socket)
	p := cfg.Providers["docker"]
	p.LockDir = t.TempDir()
	p.RegistryAuthEnv = "PULSE_TEST_AUTH"
	cfg.Providers["docker"] = p
	t.Setenv("PULSE_TEST_AUTH", "synthetic-private-auth")
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	old := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(old) })
	instances, closeAll, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	logProviders(t.Context(), logger, cfg, instances)
	if strings.Contains(logs.String(), "synthetic-private-auth") || !strings.Contains(logs.String(), "bases_complete=false") || strings.Contains(logs.String(), "free_capacity") {
		t.Fatal("incorrect or secret-bearing diagnostics")
	}
}

func TestDockerPublicListings(t *testing.T) {
	var mode atomic.Int32
	socket, _ := dockerDaemon(t, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/containers/json") || mode.Load() == 0 {
			return false
		}
		if mode.Load() == 2 {
			http.Error(w, "synthetic-secret-daemon-body", 500)
			return true
		}
		var filters map[string][]string
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
			t.Error(err)
		}
		rows := []map[string]any{}
		for _, id := range []string{"b", "a", "terminal"} {
			launch := "launch-" + id
			selected := true
			for _, filter := range filters["label"] {
				if strings.HasPrefix(filter, "pulse_launch_id=") && filter != "pulse_launch_id="+launch {
					selected = false
				}
			}
			if !selected {
				continue
			}
			labels := map[string]string{"pulse_metadata_version": "1", "pulse_managed_by": "pulse", "pulse_launch_id": launch, "pulse_jobdb_instance_id": "original", "pulse_tenant_id": "tenant", "pulse_job_id": "job-" + id}
			if id == "b" {
				labels["pulse_jobdb"] = "http://new/tenant"
			}
			if mode.Load() == 3 {
				delete(labels, "pulse_jobdb_instance_id")
			}
			state := "running"
			if id == "terminal" {
				state = "exited"
			}
			rows = append(rows, map[string]any{"Id": id, "State": state, "Labels": labels, "Created": int64(1)})
		}
		json.NewEncoder(w).Encode(rows)
		return true
	})
	cfg := dockerConfig(t, socket)
	p := cfg.Providers["docker"]
	p.LockDir = t.TempDir()
	cfg.Providers["docker"] = p
	instances, closeAll, err := Build(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	public := instances["docker"]
	mode.Store(1)
	first, err := public.List(t.Context(), api.ListRequest{PageSize: 1})
	if err != nil || len(first.Items) != 1 || first.Items[0].ID != "a" || first.Items[0].Job.JobDB != "http://localhost/tenant" || first.Items[0].Refs == nil || first.Items[0].CreatedAt == nil || first.NextPageToken == "" {
		t.Fatalf("legacy first page: %+v %v", first, err)
	}
	second, err := public.List(t.Context(), api.ListRequest{PageSize: 1, PageToken: first.NextPageToken})
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != "b" || second.Items[0].Job.JobDB != "http://new/tenant" || second.NextPageToken != "" {
		t.Fatalf("current endpoint second page: %+v %v", second, err)
	}
	filtered, err := public.List(t.Context(), api.ListRequest{LaunchID: "launch-b"})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].LaunchID != "launch-b" {
		t.Fatal("filter lost", filtered, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := public.List(canceled, api.ListRequest{}); err == nil {
		t.Fatal("cancellation ignored")
	}
	for _, failureMode := range []int32{2, 3} {
		mode.Store(failureMode)
		page, err := public.List(t.Context(), api.ListRequest{})
		if err == nil || len(page.Items) != 0 || strings.Contains(err.Error(), "synthetic-secret-daemon-body") {
			t.Fatal("failed public page or daemon secret exposed", err)
		}
	}
}

func TestDockerExplicitZeroOverheadYAML(t *testing.T) {
	for _, value := range []string{"0", "0Mi", "0.0Ki"} {
		cfg, err := config.Parse([]byte("providers: {docker: {type: docker, per_container_memory_overhead: '" + value + "'}}\ntargets: [{jobdb: 'http://localhost/tenant', launch_services: [{name: docker, priority: 1}]}]"))
		if err != nil {
			t.Fatal(value, err)
		}
		settings, err := dockerSettings(cfg.Providers["docker"], cfg)
		if err != nil || settings.Overhead != 0 {
			t.Fatal("explicit zero changed", value, err)
		}
	}
}
