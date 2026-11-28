package providers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/controller"
	"github.com/colony-2/pulse/internal/httpapi"
	"github.com/colony-2/pulse/internal/scheduler"
)

const dynamicTestYAML = `
dynamic_provider:
  listen: "127.0.0.1:0"
  tls_cert: server.pem
  tls_key: server-key.pem
  auth: {client_ca: ca.pem}
  state_file: providers.json
targets:
  - jobdb: https://jobdb.example.test/tenant
    dynamic_provider_priority: 1
`

type dynamicTestCA struct {
	cert *x509.Certificate
	key  ed25519.PrivateKey
	der  []byte
}

func newDynamicTestCA(t *testing.T) dynamicTestCA {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dynamic-test-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return dynamicTestCA{cert: cert, key: key, der: der}
}

func (ca dynamicTestCA) issue(t *testing.T, name string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: ca.cert.NotBefore, NotAfter: ca.cert.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, public, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func dynamicTestConfig(t *testing.T, fixed bool) (*config.Config, dynamicTestCA) {
	t.Helper()
	data := dynamicTestYAML
	if fixed {
		data += `
reverse: {listen: "127.0.0.1:0", transport: http}
providers:
  configured: {type: reverse, token_env: PULSE_TEST_STATIC_REVERSE_KEY}
`
	}
	cfg, err := config.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseDir = t.TempDir()
	ca := newDynamicTestCA(t)
	cert := ca.issue(t, "collector.example.test", x509.ExtKeyUsageServerAuth)
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM := func(name, kind string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfg.BaseDir, name), pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writePEM("server.pem", "CERTIFICATE", cert.Certificate[0])
	writePEM("server-key.pem", "PRIVATE KEY", key)
	writePEM("ca.pem", "CERTIFICATE", ca.der)
	return cfg, ca
}

func (ca dynamicTestCA) runOptions(t *testing.T, address, name string, cert tls.Certificate) sdk.RunOptions {
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	return sdk.RunOptions{
		Address: address, AuthMode: sdk.ReverseAuthMTLS, StartupTimeout: 3 * time.Second,
		Init:  sdk.Initialization{Name: name, BaseDir: t.TempDir()},
		Serve: sdk.ServeOptions{Implementation: "dynamic-test", Version: "1"},
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "collector.example.test",
			NextProtos: []string{sdk.ReverseALPN}, Certificates: []tls.Certificate{cert},
		},
	}
}

type dynamicTestWorker struct{ submitted chan api.Launch }

func (dynamicTestWorker) GetCapabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "ubuntu-26.04-arm64"}}}, nil
}

func (dynamicTestWorker) List(context.Context, api.ListRequest) (api.ListResponse, error) {
	return api.ListResponse{Items: []api.Instance{}}, nil
}

func (worker dynamicTestWorker) Submit(_ context.Context, launches []api.Launch, _ api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	worker.submitted <- launches[0]
	return []api.Submission{{LaunchID: launches[0].LaunchID, Status: api.Accepted}}, nil, nil
}

func runDynamicTestWorker(t *testing.T, options sdk.RunOptions, worker dynamicTestWorker) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- sdk.Run(ctx, func(context.Context, sdk.Init) (api.Provider, sdk.Cleanup, error) {
			return worker, nil, nil
		}, options)
	}()
	t.Cleanup(cancel)
	return func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("stop provider: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("provider did not stop")
		}
	}
}

func awaitDynamicTest(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDynamicReverseRegistrationAndRetention(t *testing.T) {
	cfg, ca := dynamicTestConfig(t, true)
	if _, err := StartDynamicReverse(t.Context(), cfg, true); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(cfg.BaseDir, cfg.DynamicProvider.StateFile)
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check mode changed registry: %v", err)
	}
	const identity = "worker-1"
	const staticKey = "static-provider-integration-key"
	t.Setenv("PULSE_TEST_STATIC_REVERSE_KEY", staticKey)
	static, err := StartReverse(t.Context(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = static.Close() })
	var clock atomic.Int64
	start := time.Now().UTC().Truncate(time.Second)
	now := func() time.Time { return start.Add(time.Duration(clock.Load())) }
	startServer := func() *DynamicReverseServer {
		t.Helper()
		server, err := startDynamicReverse(t.Context(), cfg, false, now)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		return server
	}
	server := startServer()
	cert := ca.issue(t, identity, x509.ExtKeyUsageClientAuth)
	options := ca.runOptions(t, server.Address, identity, cert)
	withoutCert := options
	withoutCert.TLSConfig = options.TLSConfig.Clone()
	withoutCert.TLSConfig.Certificates = nil
	withoutCert.TLSConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	for _, attempt := range []sdk.RunOptions{
		withoutCert,
		ca.runOptions(t, server.Address, identity, newDynamicTestCA(t).issue(t, identity, x509.ExtKeyUsageClientAuth)),
		ca.runOptions(t, server.Address, "unexpected-identity", cert),
		ca.runOptions(t, server.Address, "configured", ca.issue(t, "configured", x509.ExtKeyUsageClientAuth)),
	} {
		err := sdk.Run(t.Context(), func(context.Context, sdk.Init) (api.Provider, sdk.Cleanup, error) {
			t.Error("rejected connection reached the provider factory")
			return nil, nil, errors.New("unexpected initialization")
		}, attempt)
		if err == nil {
			t.Fatalf("accepted rejected credentials for %s", attempt.Init.Name)
		}
	}
	if err := sdk.Run(t.Context(), func(context.Context, sdk.Init) (api.Provider, sdk.Cleanup, error) {
		return nil, nil, errors.New("valid authentication but failed initialization")
	}, options); err == nil {
		t.Fatal("provider unexpectedly initialized")
	}
	if got := server.Providers(); len(got) != 0 {
		t.Fatalf("rejected or failed connections registered providers: %v", got)
	}
	worker := dynamicTestWorker{submitted: make(chan api.Launch, 1)}
	staticWorker := dynamicTestWorker{submitted: make(chan api.Launch, 1)}
	stopStatic := runDynamicTestWorker(t, sdk.RunOptions{
		Address: static.Address, AuthMode: sdk.ReverseAuthMutualKey, Transport: sdk.ReverseTransportHTTP,
		MutualKey: []byte(staticKey), Init: sdk.Initialization{Name: "configured", BaseDir: t.TempDir()},
		Serve: options.Serve, StartupTimeout: options.StartupTimeout,
	}, staticWorker)
	defer stopStatic()
	awaitDynamicTest(t, "static provider on its unchanged listener", func() bool {
		snapshot, err := static.Snapshot()
		return err == nil && len(snapshot.Providers) == 1 && snapshot.Providers[0].ID == "configured"
	})
	ctl := dynamicTestController(cfg, static, server)
	if err := ctl.Once(t.Context()); err != nil {
		t.Fatalf("poll without registered providers: %v", err)
	}
	web, err := httpapi.New(ctl, "test")
	if err != nil {
		t.Fatal(err)
	}
	handler := web.Handler()
	capabilities := func() (int, string) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/providers/"+identity+"/capabilities", nil))
		var result struct{ Discovery string }
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return response.Code, result.Discovery
	}
	stop := runDynamicTestWorker(t, options, worker)
	awaitDynamicTest(t, "unconfigured provider in diagnostics", func() bool {
		code, discovery := capabilities()
		return code == http.StatusOK && discovery == "known"
	})
	staticTarget := config.Target{Services: []config.Service{{Name: "configured", Priority: 1}}}
	if _, services := ctl.LaunchServices(staticTarget, ctl.ProviderSnapshot()); len(services) != 1 || services[0].Name != "configured" {
		t.Fatalf("a target without dynamic opt-in included a dynamic provider: %v", services)
	}
	if err := ctl.Once(t.Context()); err != nil {
		t.Fatalf("poll with a registered provider: %v", err)
	}
	select {
	case launch := <-worker.submitted:
		if launch.Job.JobID != "dynamic-job" {
			t.Fatalf("unexpected launch: %v", launch.Job.JobID)
		}
	default:
		t.Fatal("controller did not submit a job to the dynamic provider")
	}
	stop()
	awaitDynamicTest(t, "disconnected provider retained", func() bool {
		code, discovery := capabilities()
		return code == http.StatusOK && discovery == "unavailable"
	})
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Store(int64(2 * 24 * time.Hour))
	server = startServer()
	if backend := server.Providers()[identity]; backend == nil {
		t.Fatal("provider was not restored after restart")
	} else if _, err := backend.GetCapabilities(t.Context()); !errors.Is(err, sdk.ErrUnavailable) {
		t.Fatalf("restored provider should be offline: %v", err)
	}
	t.Log("restored the offline registration from disk after restart")
	stop = runDynamicTestWorker(t, ca.runOptions(t, server.Address, identity, cert), worker)
	awaitDynamicTest(t, "reconnection persisted with a newer timestamp", func() bool {
		return dynamicRegistryFile(t, state)[identity].Equal(now())
	})
	stop()
	awaitDynamicTest(t, "reconnected session retired", func() bool {
		snapshot, err := server.collector.Snapshot()
		return err == nil && len(snapshot.Providers) == 0
	})
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(72*time.Hour - time.Nanosecond))
	server = startServer()
	if server.Providers()[identity] == nil {
		t.Fatal("reconnect did not retain the provider for three days")
	}
	clock.Add(1)
	if server.Providers()[identity] != nil {
		t.Fatal("offline provider survived the three-day expiry")
	}
	if err := server.registry.checkpoint(); err != nil {
		t.Fatal(err)
	}
	if len(dynamicRegistryFile(t, state)) != 0 || len(static.Providers) != 1 {
		t.Fatal("expiry failed to remove only the dynamic provider")
	}
}

func TestDynamicReverseConnectedProviderDoesNotExpire(t *testing.T) {
	cfg, ca := dynamicTestConfig(t, false)
	start := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	now := func() time.Time { return start.Add(time.Duration(clock.Load())) }
	server, err := startDynamicReverse(t.Context(), cfg, false, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	const name = "long-lived"
	stop := runDynamicTestWorker(t, ca.runOptions(t, server.Address, name, ca.issue(t, name, x509.ExtKeyUsageClientAuth)), dynamicTestWorker{})
	awaitDynamicTest(t, "connected provider", func() bool { return server.Providers()[name] != nil })
	clock.Add(int64(4 * 24 * time.Hour))
	if server.Providers()[name] == nil {
		t.Fatal("expired a connected provider")
	}
	if err := server.registry.checkpoint(); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(cfg.BaseDir, cfg.DynamicProvider.StateFile)
	if seen := dynamicRegistryFile(t, state)[name]; !seen.Equal(now()) {
		t.Fatalf("checkpoint did not refresh the connected provider: %v", seen)
	}
	clock.Add(int64(4 * 24 * time.Hour))
	stop()
	awaitDynamicTest(t, "long-lived session retired", func() bool {
		snapshot, err := server.collector.Snapshot()
		return err == nil && len(snapshot.Providers) == 0
	})
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if seen := dynamicRegistryFile(t, state)[name]; !seen.Equal(now()) {
		t.Fatalf("disconnect did not refresh the registration: %v", seen)
	}
}

func dynamicRegistryFile(t *testing.T, path string) map[string]time.Time {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]time.Time
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	return records
}

type dynamicTestJobDB struct{}

func (dynamicTestJobDB) List(context.Context, string, string, []c2j.Route) (c2j.Page, error) {
	return c2j.Page{Jobs: []c2j.Job{{ID: "dynamic-job", Tenant: "tenant", Execution: &c2j.View{Status: "unresolved"}}}}, nil
}

func (dynamicTestJobDB) Claim(_ context.Context, _ string, job c2j.Job, _ string, _ time.Duration, _ []c2j.Route) (*c2j.Claimed, error) {
	return &c2j.Claimed{Job: job, Encoded: []byte("fixture-lease")}, nil
}

func dynamicTestController(cfg *config.Config, static *ReverseServer, dynamic *DynamicReverseServer) *controller.Controller {
	return &controller.Controller{
		Config: cfg, Providers: static.Providers, DynamicProviders: dynamic.Providers,
		Scheduler: scheduler.New(cfg.Cool, cfg.Call), Lister: dynamicTestJobDB{}, Claimer: dynamicTestJobDB{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestDynamicReverseReportsCleanupFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		cleanup sdk.Cleanup
	}{
		{name: "worker-error", cleanup: func(context.Context) error { return errors.New("worker cleanup failed") }},
		{name: "worker-timeout", cleanup: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, ca := dynamicTestConfig(t, false)
			server, err := StartDynamicReverse(t.Context(), cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			options := ca.runOptions(t, server.Address, test.name, ca.issue(t, test.name, x509.ExtKeyUsageClientAuth))
			options.Serve.ShutdownTimeout = 200 * time.Millisecond
			workerDone := make(chan error, 1)
			go func() {
				workerDone <- sdk.Run(t.Context(), func(context.Context, sdk.Init) (api.Provider, sdk.Cleanup, error) {
					return dynamicTestWorker{}, test.cleanup, nil
				}, options)
			}()
			awaitDynamicTest(t, "worker registered", func() bool { return server.Providers()[test.name] != nil })
			if err := server.Close(); !errors.Is(err, sdk.ErrCleanup) {
				t.Fatalf("shutdown should report the worker's cleanup failure, got %v", err)
			}
			select {
			case err := <-workerDone:
				if !errors.Is(err, sdk.ErrCleanup) {
					t.Fatalf("worker did not report failed cleanup: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not stop")
			}
		})
	}
}

func TestDynamicReverseRequiresValidRegistryAndCredentials(t *testing.T) {
	cfg, _ := dynamicTestConfig(t, false)
	if _, err := StartDynamicReverse(t.Context(), &config.Config{}, false); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(cfg.BaseDir, cfg.DynamicProvider.StateFile)
	if err := os.WriteFile(state, []byte("invalid registry"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := StartDynamicReverse(t.Context(), cfg, false); err == nil {
		t.Fatal("invalid registry should prevent startup")
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	cfg.DynamicProvider.Auth.ClientCA = "missing.pem"
	if _, err := StartDynamicReverse(t.Context(), cfg, true); err == nil {
		t.Fatal("missing CA should fail configuration check")
	}
}
