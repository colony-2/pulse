package providers_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colony-2/c2j/pkg/contextual"
	"github.com/colony-2/c2j/pkg/jobdbschema"
	"github.com/colony-2/c2j/pkg/ops"
	"github.com/colony-2/c2j/pkg/recipe"
	"github.com/colony-2/c2j/pkg/starter"
	"github.com/colony-2/c2j/pkg/worker/commandop"
	"github.com/colony-2/c2j/pkg/workflowctl"
	"github.com/colony-2/jobdb/pkg/jobdb"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/sqlite"
	jobworkflow "github.com/colony-2/jobdb/pkg/workflow"
	dockerprovider "github.com/colony-2/pulse/internal/providers/docker"
)

const c2jIntegrationImage = "ghcr.io/colony-2/shai-mega:latest"

// TestDockerC2JIntegration exercises the installed executor, not a c2j test
// harness or a binary copied out of the module cache. It deliberately runs in
// ordinary go test ./..., on both Docker Desktop and local Linux daemons.
func TestDockerC2JIntegration(t *testing.T) {
	ops.Register(commandop.GetOp())
	socket, err := dockerprovider.ResolveSocket("")
	if err != nil {
		t.Fatal(err)
	}
	// A cold pull includes downloading and unpacking a large development image.
	// Give preparation its own budget so slow registry/VM I/O cannot consume
	// the time reserved for executing the actual jobs.
	pullCtx, stopPull := context.WithTimeout(t.Context(), 30*time.Minute)
	started := time.Now()
	t.Logf("pulling %s (image preparation timeout: 30m)", c2jIntegrationImage)
	pullOutput, pullErr := exec.CommandContext(pullCtx, "docker", "--host", socket, "pull", c2jIntegrationImage).CombinedOutput()
	pullContextErr := pullCtx.Err()
	stopPull()
	if pullErr != nil {
		t.Fatalf("prepare executor image after %s: %v (context: %v)\n%s", time.Since(started), pullErr, pullContextErr, pullOutput)
	}
	t.Logf("executor image ready after %s; starting 8m execution budget", time.Since(started))
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	docker := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	imageID := docker(t, "image", "inspect", "--format", "{{.Id}}", c2jIntegrationImage)
	t.Logf("executor image: %s", docker(t, "image", "inspect", "--format", "{{json .RepoDigests}}", c2jIntegrationImage))
	t.Log(docker(t, "run", "--rm", "--entrypoint", "c2j", c2jIntegrationImage, "version"))
	binary := filepath.Join(t.TempDir(), "pulse")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/pulse").CombinedOutput(); err != nil {
		t.Fatalf("build Pulse: %v\n%s", err, out)
	}

	for _, scenario := range []string{"success and heartbeat", "host gateway connectivity", "recipe failure", "recipe timeout", "lease import transport failure", "heartbeat transport failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDockerJobDB(t, ctx, docker)
			run := "test \"$(cat README)\" = fixture; echo saved > result.txt; echo container-result"
			extra := ""
			switch scenario {
			case "success and heartbeat":
				// Wait for a real background renewal before allowing the command to
				// complete. There is no sleep-based assumption about VM startup speed.
				run = "curl --fail --silent '" + f.url + "/gate'; " + run
			case "host gateway connectivity":
				_, port, err := net.SplitHostPort(strings.TrimPrefix(f.url, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				gateway := "http://" + net.JoinHostPort("host.docker.internal", port)
				run = "test \"$(curl --fail --silent '" + gateway + "/healthz')\" = pulse-jobdb-fixture; " + run
			case "recipe failure":
				run = "echo intentional-recipe-failure >&2; exit 23"
			case "recipe timeout":
				extra = "timeout: 1s\n"
				run = "sleep 300; echo must-not-complete"
			case "lease import transport failure":
				f.dropImport.Store(true)
				run = "curl --fail '" + f.url + "/forbidden'"
			case "heartbeat transport failure":
				f.dropHeartbeat.Store(true)
				run = "curl --fail --silent '" + f.url + "/gate'; curl --fail '" + f.url + "/forbidden'"
			}
			run = "set -eu; " + run
			rec, err := recipe.LoadRecipeFromString([]byte("id: docker-executor\n" + extra + "op: command_execution\ninputs:\n  run: " + fmt.Sprintf("%q", run) + "\noutputs:\n  result: '${{ op.outputs.stdout }}'\n"))
			if err != nil {
				t.Fatal(err)
			}
			engine, err := jobworkflow.NewEngineBuilder().WithRuntime(f.backend).BuildEngine()
			if err != nil {
				t.Fatal(err)
			}
			key, err := starter.StartRecipeJob(ctx, workflowctl.StartJob{
				TenantId: "docker-test", RecipeName: rec.GetMetadata().ID, GitRef: f.hash,
				JobContext: contextual.JobContext{
					Workflow: contextual.WorkflowContext{ProjectId: "docker-test", CellName: "."},
					GitBase:  contextual.GitBaseContext{BaseRepo: f.url + "/git/repo.git", BaseRef: f.hash, ResolvedBaseHash: f.hash},
				},
			}, jobdbschema.WorkflowEngine{Engine: engine, Registry: f.backend}, *rec)
			if err != nil {
				t.Fatal(err)
			}

			// This is exactly the no-config installed CLI invocation. No executor,
			// provider, image, socket, platform, resource, or environment overrides.
			cmd := exec.CommandContext(ctx, binary, "run", "--once")
			cmd.Dir = t.TempDir()
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				if name != "PULSE_CONFIG" && name != "C2J_JOBDB" {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "C2J_JOBDB="+f.url+"/docker-test")
			var out []byte
			// Register before launch so an uncertain/failed submission is cleaned up too.
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
				defer stop()
				list, err := exec.CommandContext(cleanup, "docker", "--host", socket, "ps", "-aq", "--filter", "label=pulse_job_id="+key.JobId).CombinedOutput()
				if err != nil {
					t.Errorf("list cleanup containers: %v: %s", err, list)
					return
				}
				if t.Failed() {
					t.Logf("Pulse logs:\n%s", out)
				}
				for _, id := range strings.Fields(string(list)) {
					if t.Failed() {
						logs, _ := exec.CommandContext(cleanup, "docker", "--host", socket, "logs", id).CombinedOutput()
						t.Logf("c2j container logs:\n%s", logs)
					}
					if output, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", id).CombinedOutput(); err != nil {
						t.Errorf("cleanup: %v\n%s", err, output)
					}
				}
			})
			out, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Pulse: %v\n%s", err, out)
			}
			ids := strings.Fields(docker(t, "ps", "-aq", "--filter", "label=pulse_job_id="+key.JobId))
			if len(ids) != 1 {
				t.Fatalf("expected one launched c2j container, got %v\nPulse: %s", ids, out)
			}
			id := ids[0]
			code := docker(t, "wait", id)
			logs := docker(t, "logs", id)
			info, err := f.backend.GetJob(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			data, resultErr := info.Data.GetData()
			t.Logf("c2j exit=%s job=%s claims=%d renewals=%d", code, info.Status, f.claims.Load(), f.renewals.Load())
			if f.claims.Load() != 1 {
				t.Fatalf("lease was not handed off exactly once: %d acquisitions", f.claims.Load())
			}
			if f.forbidden.Load() != 0 {
				t.Fatal("executor continued after losing authority")
			}
			switch scenario {
			case "success and heartbeat", "host gateway connectivity":
				minimumRenewals := int32(2) // Import and runner validation.
				if scenario == "success and heartbeat" {
					minimumRenewals = 3 // Also require a background renewal.
				}
				if code != "0" || resultErr != nil || info.Status != jobdb.JobStatusCompleted || !strings.Contains(string(data), "container-result") || f.renewals.Load() < minimumRenewals {
					t.Fatalf("job did not complete through the supplied lease: exit=%s status=%s data=%s\n%s", code, info.Status, data, logs)
				}
				chapters, err := f.backend.ListChapters(ctx, jobdb.ListChaptersRequest{JobKey: key})
				if err != nil {
					t.Fatal(err)
				}
				// The initial chapter only contains the submitted recipe; execution must
				// add persisted chapters/artifacts of its own.
				if len(chapters) < 2 {
					t.Fatal("executor did not persist execution chapters")
				}
				artifacts := 0
				for _, chapter := range chapters[1:] {
					for _, artifact := range chapter.Artifacts {
						reader, err := f.backend.OpenArtifact(ctx, jobdb.ArtifactRef{JobKey: key, Ordinal: chapter.Ordinal, Name: artifact.Name, Digest: artifact.Digest})
						if err != nil {
							t.Fatal(err)
						}
						stream, err := reader.Open()
						if err != nil {
							t.Fatal(err)
						}
						hash := sha256.New()
						size, readErr := io.Copy(hash, stream)
						closeErr := stream.Close()
						if readErr != nil || closeErr != nil || size != artifact.Size || fmt.Sprintf("%x", hash.Sum(nil)) != strings.TrimPrefix(artifact.Digest, "sha256:") {
							t.Fatalf("corrupt persisted artifact %s: size=%d read=%v close=%v", artifact.Name, size, readErr, closeErr)
						}
						artifacts++
					}
				}
				if artifacts == 0 {
					t.Fatal("executor did not persist execution artifacts")
				}
				// A subsequent controller pass must not claim or launch the
				// completed job again, even after the original Pulse process exits.
				again := exec.CommandContext(ctx, binary, "run", "--once")
				again.Dir, again.Env = cmd.Dir, cmd.Env
				if output, err := again.CombinedOutput(); err != nil {
					t.Fatalf("second Pulse pass: %v\n%s", err, output)
				}
				if ids := strings.Fields(docker(t, "ps", "-aq", "--filter", "label=pulse_job_id="+key.JobId)); len(ids) != 1 || f.claims.Load() != 1 {
					t.Fatal("completed job was claimed or launched again")
				}
			case "recipe failure", "recipe timeout":
				want := "command execution failed: exit status 23"
				if scenario == "recipe timeout" {
					want = "job total timed out after 1s"
				}
				if code == "0" || resultErr == nil || info.Status != jobdb.JobStatusCompleted || !strings.Contains(logs+fmt.Sprint(resultErr), want) {
					t.Fatalf("missing c2j failure outcome %q: exit=%s data=%s\n%s", want, code, data, logs)
				}
			case "lease import transport failure":
				if code == "0" || info.Status == jobdb.JobStatusCompleted || !strings.Contains(logs, "import supplied lease: lease renewal transport failed (HTTP status 0)") {
					t.Fatalf("missing lease import transport failure: exit=%s status=%s\n%s", code, info.Status, logs)
				}
				chapters, err := f.backend.ListChapters(ctx, jobdb.ListChaptersRequest{JobKey: key})
				if err != nil || len(chapters) != 1 {
					t.Fatalf("failed lease import must not execute recipe steps: chapters=%d error=%v", len(chapters), err)
				}
			case "heartbeat transport failure":
				if code == "0" || info.Status == jobdb.JobStatusCompleted || f.renewals.Load() < 3 || !strings.Contains(logs, "lease renewal") {
					t.Fatalf("lost heartbeat did not stop execution: exit=%s status=%s\n%s", code, info.Status, logs)
				}
			}
			var inspected []struct {
				Config struct {
					Image                string
					Entrypoint, Cmd, Env []string
				}
				HostConfig struct{ Binds []string }
			}
			if err := json.Unmarshal([]byte(docker(t, "inspect", id)), &inspected); err != nil || len(inspected) != 1 {
				t.Fatalf("inspect: %v", err)
			}
			c := inspected[0]
			if c.Config.Image != imageID || strings.Join(c.Config.Entrypoint, " ") != "c2j" || !strings.Contains(strings.Join(c.Config.Cmd, " "), "run with-lease") || len(c.HostConfig.Binds) != 0 || !strings.Contains(strings.Join(c.Config.Env, "\n"), "C2J_EXECUTION_IMAGE="+c2jIntegrationImage) {
				t.Fatal("executor did not use the unmodified default image and direct c2j entrypoint")
			}
			if strings.Contains(strings.Join(c.Config.Env, "\n")+strings.Join(c.Config.Cmd, " "), "leaseToken") {
				t.Fatal("lease capability leaked into container metadata")
			}
		})
	}
}

type dockerJobDB struct {
	backend                     *sqlite.Runtime
	url, hash                   string
	claims, renewals, forbidden atomic.Int32
	dropImport, dropHeartbeat   atomic.Bool
	gate                        chan struct{}
	release                     sync.Once
}

func newDockerJobDB(t *testing.T, ctx context.Context, docker func(*testing.T, ...string) string) *dockerJobDB {
	t.Helper()
	backend, err := sqlite.NewFromConfig(ctx, sqlite.Config{DBPath: filepath.Join(t.TempDir(), "jobs.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f := &dockerJobDB{backend: backend, gate: make(chan struct{})}
	t.Cleanup(func() { f.release.Do(func() { close(f.gate) }) })
	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "README")
	git("-c", "user.name=Pulse Integration", "-c", "user.email=pulse@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	f.hash = git("rev-parse", "HEAD")
	gitRoot := t.TempDir()
	git("clone", "--bare", repo, filepath.Join(gitRoot, "repo.git"))
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	api := remote.NewServer(backend)
	mux := http.NewServeMux()
	// Serve Git's real smart HTTP protocol, including the shallow fetches c2j
	// uses. A static .git directory would not support those fetches.
	mux.Handle("/git/", &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Root: "/git", Env: []string{"GIT_PROJECT_ROOT=" + gitRoot, "GIT_HTTP_EXPORT_ALL=1"}})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "pulse-jobdb-fixture") })
	mux.HandleFunc("/gate", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-f.gate:
		case <-r.Context().Done():
		case <-ctx.Done():
		}
	})
	mux.HandleFunc("/forbidden", func(w http.ResponseWriter, _ *http.Request) { f.forbidden.Add(1) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lease") || r.URL.Path == "/v1/jobs/poll" {
			f.claims.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/keepalive") {
			n := f.renewals.Add(1)
			if f.dropImport.Load() || (f.dropHeartbeat.Load() && n >= 3) {
				// Drop the TCP connection, producing the reported HTTP-status-0 failure
				// from the real c2j binary, rather than substituting an HTTP error code.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			api.ServeHTTP(w, r)
			if n >= 3 {
				f.release.Do(func() { close(f.gate) })
			}
			return
		}
		api.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(mux)
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(func() {
		f.release.Do(func() { close(f.gate) })
		server.Close()
	})
	// A JobDB URI has to be reachable by both the native controller and the
	// container. Select a host interface by probing from the actual image; do
	// not assume localhost denotes the host inside a Docker VM. This also works
	// with Colima and nested Linux Docker without hard-coded gateway addresses.
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	var candidates []string
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() && ip.IsGlobalUnicast() {
			candidates = append(candidates, fmt.Sprintf("http://%s:%d", ip, port))
		}
	}
	if len(candidates) == 0 {
		t.Fatal("no host IPv4 interface for the container's JobDB connection")
	}
	script := `for url do if [ "$(curl --noproxy '*' --silent --max-time 3 "$url/healthz")" = pulse-jobdb-fixture ]; then printf '%s' "$url"; exit 0; fi; done; exit 1`
	f.url = docker(t, append([]string{"run", "--rm", "--entrypoint", "/bin/sh", c2jIntegrationImage, "-ec", script, "probe"}, candidates...)...)
	t.Logf("real JobDB HTTP endpoint: %s", f.url)
	return f
}
