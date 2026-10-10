package providers_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
)

type dockerJobDB struct {
	backend                                 *remote.Runtime
	url, controllerURL, hash, publishedPort string
	client                                  *http.Client
	ctx                                     context.Context
}

func buildDockerJobDBFixture(t *testing.T, ctx context.Context, docker func(*testing.T, ...string) string) string {
	t.Helper()
	arch := docker(t, "image", "inspect", "--format", "{{.Architecture}}", c2jIntegrationImage)
	path := filepath.Join(t.TempDir(), "jobdb-fixture")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", path, "./testdata/jobdb-fixture")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build JobDB fixture: %v\n%s", err, out)
	}
	return path
}

func newDockerJobDB(t *testing.T, ctx context.Context, docker func(*testing.T, ...string) string, socket, binary string) *dockerJobDB {
	t.Helper()
	// Docker streams the binary, rather than assuming the daemon shares the test
	// runner's filesystem. Worker containers still use the unmodified base image.
	id := docker(t, "create", "--publish", "8080", "--entrypoint", "/tmp/jobdb-fixture", c2jIntegrationImage)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if t.Failed() {
			out, _ := exec.CommandContext(cleanup, "docker", "--host", socket, "logs", id).CombinedOutput()
			t.Logf("JobDB fixture logs:\n%s", out)
		}
		if out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", id).CombinedOutput(); err != nil {
			t.Errorf("remove fixture: %v: %s", err, out)
		}
	})
	docker(t, "cp", binary, id+":/tmp/jobdb-fixture")
	docker(t, "start", id)
	var state struct {
		NetworkSettings struct {
			Ports    map[string][]struct{ HostIP, HostPort string }
			Networks map[string]struct{ IPAddress, Gateway string }
		}
	}
	if err := json.Unmarshal([]byte(docker(t, "inspect", "--format", "{{json .}}", id)), &state); err != nil {
		t.Fatal(err)
	}
	network := state.NetworkSettings.Networks["bridge"]
	ports := state.NetworkSettings.Ports["8080/tcp"]
	if network.IPAddress == "" || len(ports) == 0 {
		t.Fatal("fixture lacks peer address or published port")
	}
	port := ports[0].HostPort
	// Host/desktop runners reach the published port on loopback. A runner in a
	// sibling container may need the daemon's gateway instead. Never assume an
	// interface address of this process belongs to the Docker host.
	candidates := []string{"http://127.0.0.1:" + port, "http://host.docker.internal:" + port}
	if gateway := defaultRouteGateway(); gateway != "" {
		candidates = append(candidates, "http://"+net.JoinHostPort(gateway, port))
	}
	if network.Gateway != "" {
		candidates = append(candidates, "http://"+net.JoinHostPort(network.Gateway, port))
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	controllerURL, err := reachableFixture(ctx, client, candidates)
	if err != nil {
		t.Fatal(err)
	}
	f := &dockerJobDB{url: "http://" + net.JoinHostPort(network.IPAddress, "8080"), controllerURL: controllerURL, publishedPort: port, client: client, ctx: ctx}
	f.backend, err = remote.New(controllerURL, client)
	if err != nil {
		t.Fatal(err)
	}
	f.hash = string(f.request(t, "GET", "hash"))
	t.Logf("JobDB fixture: controller=%s worker peer=%s published port=%s", f.controllerURL, f.url, port)
	return f
}

func reachableFixture(ctx context.Context, client *http.Client, candidates []string) (string, error) {
	// First launch must initialize SQLite and Git before HTTP becomes ready.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var failures []string
	for ctx.Err() == nil {
		failures = nil
		for _, candidate := range candidates {
			req, err := http.NewRequestWithContext(ctx, "GET", candidate+"/healthz", nil)
			if err != nil {
				return "", err
			}
			response, err := client.Do(req)
			if err == nil {
				body, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				if readErr == nil && response.StatusCode == 200 && string(body) == "pulse-jobdb-fixture" {
					return candidate, nil
				}
				err = fmt.Errorf("unexpected health response: %s", response.Status)
			}
			failures = append(failures, candidate+": "+err.Error())
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("no reachable published JobDB endpoint: %s", strings.Join(failures, "; "))
}

func TestReachableFixtureRunnerTopologies(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("unexpected probe path: %s", r.URL.Path)
		}
		io.WriteString(w, "pulse-jobdb-fixture")
	}))
	defer ready.Close()
	unrelated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "some other service on the runner's loopback")
	}))
	defer unrelated.Close()
	for _, tc := range []struct {
		name       string
		candidates []string
	}{
		{"native runner uses published loopback", []string{ready.URL}},
		{"peer runner falls back to daemon endpoint", []string{unrelated.URL, ready.URL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reachableFixture(t.Context(), ready.Client(), tc.candidates)
			if err != nil || got != ready.URL {
				t.Fatal(got, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reachableFixture(ctx, ready.Client(), []string{ready.URL}); err == nil {
		t.Fatal("ignored canceled fixture startup")
	}
}

func defaultRouteGateway() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	} // Docker Desktop's published loopback port is used.
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err == nil && len(raw) == 4 {
			return net.IPv4(raw[3], raw[2], raw[1], raw[0]).String()
		}
	}
	return ""
}

func (f *dockerJobDB) request(t *testing.T, method, action string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(f.ctx, method, f.controllerURL+"/control/"+action, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("fixture control %s: %s: %v", action, resp.Status, err)
	}
	return data
}
func (f *dockerJobDB) control(t *testing.T, action string) { t.Helper(); f.request(t, "POST", action) }
func (f *dockerJobDB) stats(t *testing.T) (out struct{ Claims, Renewals, Forbidden int32 }) {
	t.Helper()
	if err := json.Unmarshal(f.request(t, "GET", "stats"), &out); err != nil {
		t.Fatal(err)
	}
	return
}

func (f *dockerJobDB) localhostURL(t *testing.T) string {
	t.Helper()
	target, err := url.Parse(f.controllerURL)
	if err != nil {
		t.Fatal(err)
	}
	if !net.ParseIP(target.Hostname()).IsLoopback() {
		// In a sibling test runner, localhost is the runner, not the Docker host.
		// Only the controller gets this local forwarder. Workers still reach the
		// actual daemon-published port through production host-gateway translation.
		listener, err := net.Listen("tcp4", "127.0.0.1:"+f.publishedPort)
		if err != nil {
			t.Fatal(err)
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.Transport = f.client.Transport
		server := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second}
		t.Cleanup(func() { server.Close() })
		go server.Serve(listener)
	}
	return "http://localhost:" + f.publishedPort
}

func TestPeerRunnerLocalhostFixtureForward(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/docker-test" {
			t.Errorf("forwarded path: %s", r.URL.Path)
		}
		io.WriteString(w, "peer fixture response")
	}))
	defer origin.Close()
	// A DNS endpoint exercises the peer-runner path even on a native host.
	target := strings.Replace(origin.URL, "127.0.0.1", "localhost", 1)
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(reservation.Addr().String())
	reservation.Close()
	f := &dockerJobDB{controllerURL: target, publishedPort: port, client: origin.Client()}
	response, err := origin.Client().Get(f.localhostURL(t) + "/docker-test")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "peer fixture response" {
		t.Fatal(response.Status, string(body), err)
	}
}
