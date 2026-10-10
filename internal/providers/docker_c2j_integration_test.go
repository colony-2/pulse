package providers_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
	jobworkflow "github.com/colony-2/jobdb/pkg/workflow"
	dockerprovider "github.com/colony-2/pulse/internal/providers/docker"
)

const c2jIntegrationImage = "ghcr.io/colony-2/base:latest"

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

	fixtureBinary := buildDockerJobDBFixture(t, ctx, docker)

	for _, scenario := range []string{"success and heartbeat", "peer connectivity", "host gateway connectivity", "localhost JobDB", "recipe failure", "recipe timeout", "lease import transport failure", "heartbeat transport failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDockerJobDB(t, ctx, docker, socket, fixtureBinary)
			run := "test \"$(cat README)\" = fixture; echo saved > result.txt; echo container-result"
			extra := ""
			switch scenario {
			case "success and heartbeat":
				// Wait for a real background renewal before allowing the command to
				// complete. There is no sleep-based assumption about VM startup speed.
				run = pythonHTTPGet(f.url+"/gate") + "; " + run
			case "peer connectivity":
				run = "test \"$(" + pythonHTTPGet(f.url+"/healthz") + ")\" = pulse-jobdb-fixture; " + run
			case "host gateway connectivity":
				_, port, err := net.SplitHostPort(strings.TrimPrefix(f.controllerURL, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				gateway := "http://" + net.JoinHostPort("host.docker.internal", port)
				run = "test \"$(" + pythonHTTPGet(gateway+"/healthz") + ")\" = pulse-jobdb-fixture; " + run
			case "recipe failure":
				run = "echo intentional-recipe-failure >&2; exit 23"
			case "recipe timeout":
				extra = "timeout: 1s\n"
				run = "sleep 300; echo must-not-complete"
			case "lease import transport failure":
				f.control(t, "drop-import")
				run = pythonHTTPGet(f.url + "/forbidden")
			case "heartbeat transport failure":
				f.control(t, "drop-heartbeat")
				run = pythonHTTPGet(f.url+"/gate") + "; " + pythonHTTPGet(f.url+"/forbidden")
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
				if name != "PULSE_CONFIG" && name != "C2J_JOBDB" && name != "NO_PROXY" && name != "no_proxy" {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			jobDB := f.controllerURL + "/docker-test"
			if scenario == "localhost JobDB" {
				jobDB = f.localhostURL(t) + "/docker-test"
			}
			cmd.Env = append(cmd.Env, "C2J_JOBDB="+jobDB)
			endpoint, _ := url.Parse(jobDB)
			cmd.Env = append(cmd.Env, "NO_PROXY="+os.Getenv("NO_PROXY")+","+os.Getenv("no_proxy")+","+endpoint.Hostname())
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
					if output, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", id).CombinedOutput(); err != nil && !strings.Contains(string(output), "No such container") {
						t.Errorf("cleanup: %v\n%s", err, output)
					}
				}
				// Initialization is logged before worker creation, including failed
				// launches. Delete only namespaces this isolated fixture created.
				for _, match := range regexp.MustCompile(`Docker dependency cache initialized cache=([a-f0-9]{64})`).FindAllSubmatch(out, -1) {
					cacheKey := string(match[1])
					if output, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "pulse-cache-init-"+cacheKey).CombinedOutput(); err != nil {
						t.Errorf("cleanup initializer: %v: %s", err, output)
					}
					for _, kind := range []string{"nix", "tools"} {
						if output, err := exec.CommandContext(cleanup, "docker", "--host", socket, "volume", "rm", "pulse-cache-v1-"+cacheKey+"-"+kind).CombinedOutput(); err != nil {
							t.Errorf("cleanup cache: %v: %s", err, output)
						}
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
			observation := observeDocker(t, ctx, socket, id)
			f.control(t, "observed")
			code, logs := observation.finish(t, ctx)
			t.Logf("c2j container logs:\n%s", logs)
			info, err := f.backend.GetJob(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			data, resultErr := info.Data.GetData()
			t.Logf("c2j exit=%s job=%s claims=%d renewals=%d", code, info.Status, f.stats(t).Claims, f.stats(t).Renewals)
			if f.stats(t).Claims != 1 {
				t.Fatalf("lease was not handed off exactly once: %d acquisitions", f.stats(t).Claims)
			}
			if f.stats(t).Forbidden != 0 {
				t.Fatal("executor continued after losing authority")
			}
			switch scenario {
			case "success and heartbeat", "peer connectivity", "host gateway connectivity", "localhost JobDB":
				minimumRenewals := int32(2) // Import and runner validation.
				if scenario == "success and heartbeat" {
					minimumRenewals = 3 // Also require a background renewal.
				}
				if code != "0" || resultErr != nil || info.Status != jobdb.JobStatusCompleted || !strings.Contains(string(data), "container-result") || f.stats(t).Renewals < minimumRenewals {
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
				if ids := strings.Fields(docker(t, "ps", "-aq", "--filter", "label=pulse_job_id="+key.JobId)); len(ids) != 0 || f.stats(t).Claims != 1 {
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
				if code == "0" || info.Status == jobdb.JobStatusCompleted || f.stats(t).Renewals != 1 || !strings.Contains(logs, "cannot validate supplied lease") || !strings.Contains(logs, "request failed (phase exchange)") || !strings.Contains(logs, "job execution has not started") {
					t.Fatalf("missing lease import transport failure: exit=%s status=%s\n%s", code, info.Status, logs)
				}
				chapters, err := f.backend.ListChapters(ctx, jobdb.ListChaptersRequest{JobKey: key})
				if err != nil || len(chapters) != 1 {
					t.Fatalf("failed lease import must not execute recipe steps: chapters=%d error=%v", len(chapters), err)
				}
			case "heartbeat transport failure":
				if code == "0" || info.Status == jobdb.JobStatusCompleted || f.stats(t).Renewals < 3 || !strings.Contains(logs, "lease renewal") {
					t.Fatalf("lost heartbeat did not stop execution: exit=%s status=%s\n%s", code, info.Status, logs)
				}
			}
			var inspected []struct {
				Config struct {
					Image                string
					Entrypoint, Cmd, Env []string
				}
				HostConfig struct {
					Binds      []string
					AutoRemove bool
				}
			}
			if err := json.Unmarshal([]byte("["+string(observation.inspection)+"]"), &inspected); err != nil || len(inspected) != 1 {
				t.Fatalf("inspect: %v", err)
			}
			c := inspected[0]
			if scenario == "localhost JobDB" {
				want := strings.Replace(jobDB, "localhost", "host.docker.internal", 1)
				if !strings.Contains(strings.Join(c.Config.Cmd, " "), "--jobdb "+want) || !strings.Contains(string(out), "worker_jobdb") {
					t.Fatal("loopback JobDB endpoint was not adapted and logged", c.Config.Cmd, string(out))
				}
			}
			if !c.HostConfig.AutoRemove || c.Config.Image != imageID || strings.Join(c.Config.Entrypoint, " ") != "c2j" || !strings.Contains(strings.Join(c.Config.Cmd, " "), "run with-lease") || len(c.HostConfig.Binds) != 0 || !strings.Contains(strings.Join(c.Config.Env, "\n"), "C2J_EXECUTION_IMAGE="+c2jIntegrationImage) {
				t.Fatal("executor did not use the unmodified default image and direct c2j entrypoint")
			}
			if strings.Contains(strings.Join(c.Config.Env, "\n")+strings.Join(c.Config.Cmd, " "), "leaseToken") {
				t.Fatal("lease capability leaked into container metadata")
			}
		})
	}
}

// The base image deliberately supplies Python rather than the mega image's curl.
func pythonHTTPGet(address string) string {
	return "python3 -c 'import sys, urllib.request; sys.stdout.buffer.write(urllib.request.build_opener(urllib.request.ProxyHandler({})).open(sys.argv[1], timeout=300).read())' " + fmt.Sprintf("%q", address)
}
