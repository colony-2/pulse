// jobdb-fixture serves the integration test's real JobDB and Git repository in
// Docker, so test runners on a host or in a peer container use the same fixture.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/colony-2/jobdb/pkg/jobdb/runtime/remote"
	"github.com/colony-2/jobdb/pkg/jobdb/runtime/sqlite"
)

func main() {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "jobdb-fixture-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(root)
	backend, err := sqlite.NewFromConfig(ctx, sqlite.Config{DBPath: filepath.Join(root, "jobs.db")})
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Close(ctx)
	repo := filepath.Join(root, "source")
	if err := os.Mkdir(repo, 0700); err != nil {
		log.Fatal(err)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("fixture\n"), 0600); err != nil {
		log.Fatal(err)
	}
	git("add", "README")
	git("-c", "user.name=Pulse Integration", "-c", "user.email=pulse@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	hash := git("rev-parse", "HEAD")
	git("clone", "--bare", repo, filepath.Join(root, "repo.git"))
	gitPath, err := exec.LookPath("git")
	if err != nil {
		log.Fatal(err)
	}
	var claims, renewals, forbidden atomic.Int32
	var dropImport, dropHeartbeat atomic.Bool
	gate, observed := make(chan struct{}), make(chan struct{})
	var release, observe sync.Once
	mux := http.NewServeMux()
	mux.Handle("/git/", &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Root: "/git", Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "pulse-jobdb-fixture") })
	mux.HandleFunc("GET /control/hash", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, hash) })
	mux.HandleFunc("GET /control/stats", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"claims":%d,"renewals":%d,"forbidden":%d}`, claims.Load(), renewals.Load(), forbidden.Load())
	})
	mux.HandleFunc("POST /control/observed", func(w http.ResponseWriter, _ *http.Request) { observe.Do(func() { close(observed) }) })
	mux.HandleFunc("POST /control/drop-import", func(w http.ResponseWriter, _ *http.Request) { dropImport.Store(true) })
	mux.HandleFunc("POST /control/drop-heartbeat", func(w http.ResponseWriter, _ *http.Request) { dropHeartbeat.Store(true) })
	mux.HandleFunc("/gate", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/forbidden", func(w http.ResponseWriter, _ *http.Request) { forbidden.Add(1) })
	api := remote.NewServer(backend)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/lease") || r.URL.Path == "/v1/jobs/poll" {
			claims.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/keepalive") {
			n := renewals.Add(1)
			if n == 1 {
				select {
				case <-observed:
				case <-r.Context().Done():
					return
				}
			}
			if dropImport.Load() || (dropHeartbeat.Load() && n >= 3) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			api.ServeHTTP(w, r)
			if n >= 3 {
				release.Do(func() { close(gate) })
			}
			return
		}
		api.ServeHTTP(w, r)
	})
	log.Fatal(http.ListenAndServe(":8080", mux))
}
