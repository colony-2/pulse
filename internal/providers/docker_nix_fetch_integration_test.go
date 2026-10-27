package providers_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerprovider "github.com/colony-2/pulse/internal/providers/docker"
	"github.com/colony-2/pulse/pkg/compute"
)

// Use Nix's real github: fetcher against a controlled HTTPS GitHub Enterprise
// endpoint. Every lookup runs in a new, automatically removed Pulse container.
func TestDockerNixFetchCacheIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	socket, err := dockerprovider.ResolveSocket("")
	if err != nil {
		t.Fatal(err)
	}
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", append([]string{"--host", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := exec.CommandContext(ctx, "docker", "--host", socket, "image", "inspect", c2jIntegrationImage).Run(); err != nil {
		docker("pull", c2jIntegrationImage)
	}
	platform := docker("image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", c2jIntegrationImage)
	_, arch, _ := strings.Cut(platform, "/")
	binary := filepath.Join(t.TempDir(), "nix-fetch-fixture")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./testdata/nix-fetch-fixture")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	fixture := docker("create", "--entrypoint", "/tmp/nix-fetch-fixture", c2jIntegrationImage)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", fixture).CombinedOutput()
		if err != nil {
			t.Errorf("remove fixture: %v %s", err, out)
		}
	})
	docker("cp", binary, fixture+":/tmp/nix-fetch-fixture")
	docker("start", fixture)
	peer := docker("inspect", "--format", `{{(index .NetworkSettings.Networks "bridge").IPAddress}}`, fixture)
	// Wait for the fixture without depending on runner-to-container routing.
	for deadline := time.Now().Add(15 * time.Second); ; {
		if exec.CommandContext(ctx, "docker", "--host", socket, "exec", fixture, "/tmp/nix-fetch-fixture", "stats").Run() == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not start", docker("logs", fixture))
		}
		time.Sleep(100 * time.Millisecond)
	}
	cert := docker("exec", fixture, "cat", "/tmp/nix-fetch-ca.pem")
	type stats struct {
		Revision, Branches, Archives int
		Disabled                     bool
	}
	control := func(action string) stats {
		t.Helper()
		var s stats
		if err := json.Unmarshal([]byte(docker("exec", fixture, "/tmp/nix-fetch-fixture", action)), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	config := dockerprovider.Config{Socket: socket, LockDir: t.TempDir(), MaxContainers: 4, DependencyCache: dockerprovider.CacheConfig{Namespace: "fetch-test-" + compute.NewID()}}
	var p *dockerprovider.Provider
	restart := func() {
		t.Helper()
		if p != nil {
			p.Close()
		}
		p, err = dockerprovider.New(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
	}
	restart()
	volumes := map[string]bool{}
	launchIDs := []string{}
	t.Cleanup(func() {
		if p != nil {
			p.Close()
		}
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, id := range launchIDs {
			out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "ps", "-aq", "--filter", "label=pulse_launch_id="+id).CombinedOutput()
			if err != nil {
				t.Errorf("list cleanup workers: %v %s", err, out)
				continue
			}
			for _, cid := range strings.Fields(string(out)) {
				_ = exec.CommandContext(cleanup, "docker", "--host", socket, "rm", "-f", cid).Run()
			}
		}
		// Also collect volumes from initialization that failed before worker creation.
		owner, _ := json.Marshal([]string{"2", config.DependencyCache.Namespace})
		out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "volume", "ls", "-q", "--filter", fmt.Sprintf("label=pulse_cache_owner=%x", sha256.Sum256(owner))).CombinedOutput()
		if err != nil {
			t.Errorf("list cleanup volumes: %v %s", err, out)
		} else {
			for _, name := range strings.Fields(string(out)) {
				volumes[name] = true
			}
		}
		for name := range volumes {
			out, err := exec.CommandContext(cleanup, "docker", "--host", socket, "volume", "rm", name).CombinedOutput()
			if err != nil && !strings.Contains(string(out), "no such volume") {
				t.Errorf("cleanup %s: %v %s", name, err, out)
			}
		}
	})
	type result struct {
		Revision, Output, Cache string
		Timestamp               int64
	}
	type worker struct {
		id          string
		observation *dockerObservation
	}
	launch := func(tenant, ttl, mode string) worker {
		t.Helper()
		id := compute.NewID()
		launchIDs = append(launchIDs, id)
		env := map[string]string{"FETCH_REF": "github:colony/probe/main?host=" + peer, "FETCH_CA": cert, "NO_PROXY": peer, "no_proxy": peer, "NIX_SSL_CERT_FILE": "/tmp/fetch-ca.pem", "FETCH_MODE": mode}
		if ttl != "" {
			env["NIX_CONFIG"] = "tarball-ttl = " + ttl
		}
		l := compute.Launch{Request: compute.Request{LaunchID: id, Allocation: compute.Allocation{Image: c2jIntegrationImage, Platform: platform}, Metadata: map[string]string{"pulse_job_id": id, "pulse_jobdb_instance_id": "fetch-db", "pulse_tenant_id": tenant}}, Process: compute.Process{Command: []string{"/bin/sh"}, Args: []string{"-ec", nixFetchWorkerScript}, Env: env}}
		out, err := p.Submit(ctx, []compute.Launch{l})
		if err != nil || len(out) != 1 || out[0].Status != compute.Accepted {
			t.Fatalf("submit: %+v %v", out, err)
		}
		cid := docker("ps", "-aq", "--filter", "label=pulse_launch_id="+id)
		var mounts []struct{ Name, Destination string }
		if err := json.Unmarshal([]byte(docker("inspect", "--format", "{{json .Mounts}}", cid)), &mounts); err != nil {
			t.Fatal(err)
		}
		for _, m := range mounts {
			volumes[m.Name] = true
		}
		return worker{cid, observeDocker(t, ctx, socket, cid)}
	}
	finish := func(w worker) result {
		t.Helper()
		code, logs := w.observation.finish(t, ctx)
		if code != "0" {
			t.Fatalf("fetch worker exit=%s\n%s", code, logs)
		}
		var r result
		for _, line := range strings.Split(logs, "\n") {
			if strings.HasPrefix(line, "FETCH_RESULT ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "FETCH_RESULT ")), &r); err != nil {
					t.Fatal(err)
				}
			}
		}
		if r.Revision == "" || r.Output == "" || r.Timestamp == 0 {
			t.Fatalf("missing result: %s", logs)
		}
		return r
	}
	run := func(tenant, ttl, mode string) result {
		t.Helper()
		w := launch(tenant, ttl, mode)
		docker("exec", w.id, "touch", "/tmp/fetch-go")
		return finish(w)
	}
	concurrent := func(tenant, ttl string) []result {
		t.Helper()
		a, b := launch(tenant, ttl, ""), launch(tenant, ttl, "")
		docker("exec", a.id, "touch", "/tmp/fetch-go")
		docker("exec", b.id, "touch", "/tmp/fetch-go")
		return []result{finish(a), finish(b)}
	}
	assertRevision := func(r result, rev int) {
		t.Helper()
		if r.Revision != fmt.Sprintf("%040x", rev) {
			t.Fatalf("revision=%s want %d", r.Revision, rev)
		}
	}
	first := run("fresh", "", "")
	assertRevision(first, 1)
	before := control("stats")
	if before.Branches != 1 || before.Archives != 1 {
		t.Fatal("unexpected cold requests", before)
	}
	restart()
	// Make timestamp equality meaningful even with Nix's whole-second clock.
	time.Sleep(1100 * time.Millisecond)
	for range 3 {
		warm := run("fresh", "", "")
		if warm != first {
			t.Fatalf("warm cache changed result or fetch timestamp: %+v %+v", first, warm)
		}
	}
	if got := control("stats"); got.Branches != before.Branches || got.Archives != before.Archives {
		t.Fatal("warm requests", got, before)
	}
	control("disable")
	if offline := run("fresh", "", ""); offline != first {
		t.Fatal("warm unavailable-upstream result differs", offline)
	}
	if got := control("stats"); got.Branches != before.Branches || got.Archives != before.Archives {
		t.Fatal("warm worker contacted disabled upstream", got)
	}
	control("enable")
	control("advance")
	// A short explicit TTL expires the original check, independent of reads.
	for time.Now().Unix() <= first.Timestamp+2 {
		time.Sleep(100 * time.Millisecond)
	}
	refreshed := run("fresh", "2", "")
	assertRevision(refreshed, 2)
	if refreshed.Output == first.Output || refreshed.Timestamp <= first.Timestamp {
		t.Fatal("expired entry did not change", refreshed)
	}
	after := control("stats")
	if after.Branches != before.Branches+1 {
		t.Fatal("expiry did not recheck branch", after, before)
	}
	if warm := run("fresh", "", ""); warm != refreshed {
		t.Fatal("refreshed entry not reused", warm)
	}
	if got := control("stats"); got.Branches != after.Branches {
		t.Fatal("refreshed entry rechecked", got)
	}
	for _, phase := range []string{"cold", "warm", "expired"} {
		before := control("stats")
		ttl := ""
		if phase == "expired" {
			control("advance")
			ttl = "1"
			time.Sleep(2100 * time.Millisecond)
		}
		results := concurrent("concurrent", ttl)
		want := control("stats")
		for _, r := range results {
			assertRevision(r, want.Revision)
		}
		if results[0].Output != results[1].Output {
			t.Fatal("concurrent outputs differ", results)
		}
		delta := want.Branches - before.Branches
		t.Logf("%s concurrent requests: branches=%d archives=%d", phase, delta, want.Archives-before.Archives)
		if (phase == "warm" && delta != 0) || (phase != "warm" && (delta < 1 || delta > 2)) {
			t.Fatal("unexpected concurrent requests", phase, delta)
		}
	}
	// Losing source-cache objects must refetch them, not accept an unusable hit.
	before = control("stats")
	_ = run("fresh", "", "lose-source")
	_ = run("fresh", "", "")
	if got := control("stats"); got.Archives <= before.Archives {
		t.Fatal("missing source cache did not recover", got, before)
	}
	_ = run("fresh", "", "lose-all")
	before = control("stats")
	restart()
	assertRevision(run("fresh", "", ""), before.Revision)
	if got := control("stats"); got.Branches <= before.Branches {
		t.Fatal("missing fetch metadata did not recover", got, before)
	}
	before = control("stats")
	_ = run("different-tenant", "", "")
	if got := control("stats"); got.Branches <= before.Branches {
		t.Fatal("tenant reused metadata", got)
	}
	config.DependencyCache.Generation = "next"
	restart()
	before = control("stats")
	_ = run("fresh", "", "")
	if got := control("stats"); got.Branches <= before.Branches {
		t.Fatal("generation reused metadata", got)
	}
	// All metadata lives in the tools volume and follows existing idle cleanup.
	config.DependencyCache.MaxAge = time.Nanosecond
	restart()
	for name := range volumes {
		if exec.CommandContext(ctx, "docker", "--host", socket, "volume", "inspect", name).Run() == nil {
			t.Fatal("idle cache survived retirement", name)
		}
	}
}

const nixFetchWorkerScript = `
while [ ! -f /tmp/fetch-go ]; do sleep 0.05; done
printf '%s\n' "$FETCH_CA" > "$NIX_SSL_CERT_FILE"
test "$HOME" = /root
test -z "${XDG_CACHE_HOME:-}"
test ! -e /root/.cache/pulse-private
mkdir -p /root/.cache
touch /root/.cache/pulse-private
nix eval --json "$FETCH_REF#probe" > /tmp/fetch-result.json
output=$(nix build --no-link --print-out-paths --option sandbox false --option max-jobs 1 --option substituters '' "$FETCH_REF#default")
"$output/bin/probe"
python3 - <<'PY'
import json, os, sqlite3
data = json.load(open('/tmp/fetch-result.json'))
cache = os.environ['NIX_CACHE_HOME']
assert any(name.startswith('eval-cache-') and os.listdir(cache + '/' + name) for name in os.listdir(cache)), 'missing evaluation cache'
with sqlite3.connect(cache + '/fetcher-cache-v4.sqlite') as db:
    timestamps = db.execute("select timestamp from Cache where key like '%/commits/main%'").fetchall()
assert timestamps, 'missing GitHub branch cache timestamp'
data.update(Cache=cache, Timestamp=max(row[0] for row in timestamps))
print('FETCH_RESULT ' + json.dumps(data))
PY
if [ "$FETCH_MODE" = lose-source ]; then
  test -d "$NIX_CACHE_HOME/tarball-cache-v2"
  rm -rf "$NIX_CACHE_HOME/tarball-cache-v2"
fi
if [ "$FETCH_MODE" = lose-all ]; then
  rm -rf "$NIX_CACHE_HOME"
fi
`
