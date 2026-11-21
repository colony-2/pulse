package providers_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	docker "github.com/colony-2/pulse-docker"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providers"
)

const extractionSource = "6c20e087383ac7c333f562aa373c873bd5423db6"

// Produce resources with the unchanged extraction source in scratch. The small
// holding process is instrumentation; real supplied leases run in the CLI test.
func TestDockerUpgradeIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	socket, err := docker.ResolveSocket("")
	if err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", socket}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	do := func(args ...string) string {
		t.Helper()
		out, err := command(args...)
		if err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, out)
		}
		return out
	}
	platform := do("info", "--format", "{{.OSType}}/{{.Architecture}}")
	platform = strings.ReplaceAll(strings.ReplaceAll(platform, "aarch64", "arm64"), "x86_64", "amd64")
	if platform != "linux/arm64" && platform != "linux/amd64" {
		t.Fatal("unsupported daemon", platform)
	}
	do("pull", "--platform", platform, c2jIntegrationImage)
	t.Logf("historical producer source=%s image=%s platform=%s", extractionSource, do("image", "inspect", "--format", "{{json .RepoDigests}}", c2jIntegrationImage), platform)
	archive, err := archiveDockerHistory(ctx, ".", extractionSource)
	if err != nil {
		// Shallow CI checkouts need the original object. Fetch only into scratch;
		// never change the candidate checkout or its module graph.
		history := t.TempDir()
		remote, remoteErr := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
		if remoteErr != nil {
			t.Fatal("historical source unavailable", err, remoteErr)
		}
		for _, args := range [][]string{{"init", "--bare", history}, {"-C", history, "fetch", "--depth=1", strings.TrimSpace(string(remote)), extractionSource}} {
			if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
				t.Fatalf("fetch historical source: %v: %s", err, out)
			}
		}
		archive, err = exec.CommandContext(ctx, "git", "-C", history, "archive", extractionSource).Output()
		if err != nil {
			t.Fatal("archive historical source", err)
		}
	}
	source := t.TempDir()
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(source, filepath.FromSlash(header.Name))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(path, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(file, reader)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatal(copyErr, closeErr)
		}
	}
	producerDir := filepath.Join(source, "cmd", "upgrade-fixture")
	if err := os.MkdirAll(producerDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(producerDir, "main.go"), []byte(historicalDockerProducer), 0600); err != nil {
		t.Fatal(err)
	}
	lockDir := t.TempDir()
	id := fmt.Sprintf("pulse-upgrade-%d", time.Now().UnixNano())
	target := "http://localhost/upgrade"
	volumes := []string{}
	worker := ""
	unrelated := do("volume", "create", id+"-unrelated")
	volumes = append(volumes, unrelated)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		// Query by this test's unique launch even if the producer failed after create.
		out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "ps", "-aq", "--filter", "label=pulse_launch_id="+id).CombinedOutput()
		if err != nil {
			t.Errorf("list historical cleanup: %v: %s", err, out)
		}
		for _, name := range strings.Fields(string(out)) {
			if out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", name).CombinedOutput(); err != nil {
				t.Errorf("remove historical worker: %v: %s", err, out)
			}
		}
		initializers, initErr := exec.CommandContext(cleanup, "docker", "--host", socket, "ps", "-aq", "--filter", "label=pulse_cache_owner="+historicalCacheOwner(id)).CombinedOutput()
		if initErr != nil {
			t.Errorf("list upgrade initializers: %v: %s", initErr, initializers)
		}
		for _, name := range strings.Fields(string(initializers)) {
			if out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", name).CombinedOutput(); err != nil {
				t.Errorf("remove upgrade initializer: %v: %s", err, out)
			}
		}
		// Initializers/volumes are scoped by the unique namespace, including failed
		// preparation before the worker acquires mounts.
		out, _ = exec.CommandContext(cleanup, "docker", "--host", socket, "volume", "ls", "-q", "--filter", "label=pulse_cache_owner="+historicalCacheOwner(id)).CombinedOutput()
		volumes = append(volumes, strings.Fields(string(out))...)
		seen := map[string]bool{}
		for _, name := range volumes {
			if seen[name] {
				continue
			}
			seen[name] = true
			if out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "volume", "rm", name).CombinedOutput(); err != nil && !strings.Contains(string(out), "no such volume") {
				t.Errorf("remove upgrade fixture volume: %v: %s", err, out)
			}
		}
	})
	produce := exec.CommandContext(ctx, "go", "run", "-mod=readonly", "./cmd/upgrade-fixture", socket, lockDir, platform, id)
	produce.Dir = source
	produce.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local")
	out, err := produce.CombinedOutput()
	if err != nil {
		t.Fatalf("historical producer: %v\n%s", err, out)
	}
	t.Logf("historical producer exited and released ownership: %s", out)
	ids := strings.Fields(do("ps", "-q", "--filter", "label=pulse_launch_id="+id))
	if len(ids) != 1 {
		t.Fatal("historical worker not held", ids)
	}
	worker = ids[0]
	do("exec", worker, "/bin/sh", "-ec", "until [ -p /scratch/release ]; do sleep 0.1; done; test \"$(cat /var/cache/pulse/upgrade-sentinel)\" = historical")
	before := do("inspect", "--format", "{{json .Mounts}}", worker)
	var mounts []struct{ Name, Destination string }
	if err := json.Unmarshal([]byte(before), &mounts); err != nil {
		t.Fatal(err)
	}
	beforeMounts, err := dockerMountsByDestination(before)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mounts {
		if m.Name != "" {
			volumes = append(volumes, m.Name)
		}
	}
	if len(mounts) < 2 {
		t.Fatal("historical cache pair missing", before)
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(do("inspect", "--format", "{{json .Config.Labels}}", worker)), &labels); err != nil {
		t.Fatal(err)
	}
	if labels["pulse_jobdb"] != "" {
		t.Fatal("historical producer did not exercise legacy endpoint lookup")
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`providers:
  docker:
    type: docker
    socket: %q
    lock_dir: %q
    capacity: {max_containers: 1}
    dependency_cache: {namespace: %q}
targets: [{instance_id: original, jobdb: %q, launch_services: [{name: docker, priority: 1}]}]
`, socket, lockDir, id, target)))
	if err != nil {
		t.Fatal(err)
	}
	instances, closeAll, err := providers.Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	p := instances["docker"]
	page, err := p.List(ctx, api.ListRequest{LaunchID: id})
	if err != nil || len(page.Items) != 1 || page.Items[0].Job.JobDB != target || page.Items[0].Job.InstanceID != "original" || page.Items[0].Job.JobID != id || page.Items[0].State != "running" {
		t.Fatalf("historical public listing: %+v %v", page, err)
	}
	launch := api.Launch{LaunchID: id + "-new", Allocation: api.Allocation{Base: "ubuntu-26.04-" + strings.TrimPrefix(platform, "linux/")}, Job: api.JobRef{JobDB: target, InstanceID: "original", TenantID: "upgrade", JobID: "new"}, Lease: "synthetic-unused-lease"}
	results, _, err := p.Submit(ctx, []api.Launch{launch}, api.SubmitOptions{})
	if err != nil || len(results) != 1 || results[0].Status != api.NoCapacity {
		t.Fatal("historical occupied capacity lost", results, err)
	}
	if got := do("ps", "-aq", "--filter", "label=pulse_launch_id="+launch.LaunchID); got != "" {
		t.Fatal("duplicate worker created", got)
	}
	if err := p.(interface{ Maintain(context.Context) error }).Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	closeAll()
	after := do("inspect", "--format", "{{json .Mounts}}", worker)
	afterMounts, err := dockerMountsByDestination(after)
	if err != nil || !reflect.DeepEqual(beforeMounts, afterMounts) {
		t.Fatalf("upgrade changed mounts: before=%s after=%s error=%v", before, after, err)
	}
	t.Logf("historical mount fields unchanged by destination; JSON array reordered=%t", before != after)
	do("exec", worker, "/bin/sh", "-ec", "test \"$(cat /var/cache/pulse/upgrade-sentinel)\" = historical")
	do("volume", "inspect", unrelated)
	observation := observeDocker(t, ctx, socket, worker)
	do("exec", worker, "/bin/sh", "-ec", "printf 'release\\n' > /scratch/release")
	code, logs := observation.finish(t, ctx)
	if code != "0" || logs != "historical-completed" {
		t.Fatal("historical worker did not finish", code, logs)
	}
	// Contents remain in the same cache after the historical worker is gone.
	for _, m := range mounts {
		if m.Name != "" {
			do("volume", "inspect", m.Name)
		}
	}
	do("volume", "inspect", unrelated)
	t.Logf("historical worker=%s caches=%s occupied capacity/no duplicate/legacy lookup/completion/cache identity/sentinel/unrelated volume preserved", worker, before)
}

// git archive observes the caller's directory prefix. Go tests run from their
// package directory, but the historical producer needs the entire module.
func archiveDockerHistory(ctx context.Context, dir, revision string) ([]byte, error) {
	root := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	root.Dir = dir
	out, err := root.Output()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, "git", "-C", strings.TrimSpace(string(out)), "archive", revision).Output()
}

// Only instrumentation is written into the archived checkout; its backend,
// shared helpers and module files are unmodified.
const historicalDockerProducer = `package main
import (
 "context"
 "fmt"
 "os"
 "time"
 "github.com/colony-2/pulse/internal/providers/docker"
 "github.com/colony-2/pulse/pkg/compute"
)
func main() {
 ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute); defer cancel()
 socket, locks, platform, id := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
 p, err := docker.New(ctx, docker.Config{Socket: socket, LockDir: locks, Overhead: 256<<20, MaxContainers: 1, DependencyCache: docker.CacheConfig{Namespace: id}})
 if err != nil { panic(err) }; defer p.Close()
 l := compute.Launch{Request: compute.Request{LaunchID: id, Allocation: compute.Allocation{Image: "ghcr.io/colony-2/base2:latest", Platform: platform, ScratchBytes: 8<<20}, Metadata: map[string]string{"pulse_metadata_version":"1", "pulse_managed_by":"pulse", "pulse_jobdb_instance_id":"original", "pulse_tenant_id":"upgrade", "pulse_job_id":id}}, Process: compute.Process{Command: []string{"/bin/sh"}, Args: []string{"-ec", "printf historical > /var/cache/pulse/upgrade-sentinel; mkfifo /scratch/release; read -r signal < /scratch/release; test \"$signal\" = release; echo historical-completed"}}}
 results, err := p.Submit(ctx, []compute.Launch{l})
 if err != nil || len(results) != 1 || results[0].Status != compute.Accepted { panic(fmt.Sprintf("historical submit: %+v %v", results, err)) }
 fmt.Println("historical worker accepted", id)
}
`

func historicalCacheOwner(namespace string) string {
	b, _ := json.Marshal([]string{"2", namespace})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
