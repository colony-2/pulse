package docker

import (
	"context"
	"encoding/json"
	"github.com/colony-2/pulse/pkg/compute"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type daemon struct {
	mu           sync.Mutex
	containers   map[string]container
	creates      []map[string]any
	startError   bool
	createError  bool
	noLimits     bool
	inspectError int
	stdin        map[string]string
}

func TestExplicitInfrastructureDeadlinesAreNotSilentlyIgnored(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	ls := launches(requests("timeout", "deadline"))
	ls[0].TimeoutSeconds = 60
	deadline := time.Now().Add(time.Hour)
	ls[1].StartBefore = &deadline
	out, err := p.Submit(context.Background(), ls)
	if err != nil || len(out) != 2 || out[0].Status != compute.Unsupported || out[1].Status != compute.Unsupported || len(d.creates) != 0 {
		t.Fatal(out, err)
	}
}

func (d *daemon) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1.47")
	if strings.HasSuffix(path, "/attach") {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		rw.Flush()
		data, _ := io.ReadAll(rw)
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.stdin == nil {
			d.stdin = map[string]string{}
		}
		d.stdin[strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/attach")] = string(data)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	enc := json.NewEncoder(w)
	switch {
	case path == "/version":
		enc.Encode(map[string]any{"ApiVersion": "1.47"})
	case path == "/info":
		enc.Encode(map[string]any{"ID": "test", "NCPU": 8, "MemTotal": int64(16 << 30), "OSType": "linux", "Architecture": "arm64", "MemoryLimit": !d.noLimits, "SwapLimit": !d.noLimits, "CpuCfsQuota": !d.noLimits})
	case strings.HasPrefix(path, "/images/"):
		enc.Encode(map[string]any{"Id": "sha256:config", "Os": "linux", "Architecture": "arm64", "RepoDigests": []string{}})
	case path == "/containers/json":
		out := []map[string]string{}
		for id := range d.containers {
			out = append(out, map[string]string{"Id": id})
		}
		enc.Encode(out)
	case path == "/containers/create":
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			panic(e)
		}
		d.creates = append(d.creates, body)
		if d.createError {
			w.WriteHeader(500)
			return
		}
		labels := map[string]string{}
		for k, v := range body["Labels"].(map[string]any) {
			labels[k] = v.(string)
		}
		id := labels["pulse_launch_id"]
		var c container
		c.ID = id
		c.Config.Labels = labels
		raw, _ := json.Marshal(body["HostConfig"])
		json.Unmarshal(raw, &c.HostConfig)
		c.State.Status = "created"
		d.containers[id] = c
		enc.Encode(map[string]any{"Id": id})
	case strings.HasSuffix(path, "/start"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/start")
		c := d.containers[id]
		c.State.Status = "running"
		d.containers[id] = c
		if d.startError {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(204)
		}
	case strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		if id == "a" && d.inspectError != 0 {
			if d.inspectError == http.StatusNotFound {
				delete(d.containers, id)
			}
			w.WriteHeader(d.inspectError)
			return
		}
		enc.Encode(d.containers[id])
	default:
		w.WriteHeader(404)
	}
}
func testProvider(t *testing.T, d *daemon) (*Provider, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(d.serve))
	p, e := open(context.Background(), Config{CPUMillis: 2000, MemoryBytes: 4 << 30, MaxContainers: 2}, &engine{base: s.URL, client: s.Client()}, false)
	if e != nil {
		t.Fatal(e)
	}
	return p, s
}
func requests(ids ...string) []compute.Request {
	rs := []compute.Request{}
	for _, id := range ids {
		rs = append(rs, compute.Request{LaunchID: id, Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: "runner:1", Platform: "linux/arm64"}, Metadata: map[string]string{"pulse_job_id": id}})
	}
	return rs
}
func launches(rs []compute.Request) []compute.Launch {
	out := []compute.Launch{}
	for _, r := range rs {
		out = append(out, compute.Launch{Request: r, Process: compute.Process{Command: []string{"c2j"}, Args: []string{}, Env: map[string]string{}}})
	}
	return out
}
func TestConcurrentBatchesAndRestart(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	a := launches(requests("a", "b"))
	b := launches(requests("c", "d"))
	results := make(chan []compute.Submission, 2)
	for _, ls := range [][]compute.Launch{a, b} {
		go func(ls []compute.Launch) {
			r, e := p.Submit(context.Background(), ls)
			if e != nil {
				t.Error(e)
			}
			results <- r
		}(ls)
	}
	accepted, full := 0, 0
	for range 2 {
		for _, r := range <-results {
			if r.Status == compute.Accepted {
				accepted++
			}
			if r.Status == compute.NoCapacity {
				full++
			}
		}
	}
	if accepted != 2 || full != 2 {
		t.Fatal(accepted, full)
	}
	for _, body := range d.creates {
		h := body["HostConfig"].(map[string]any)
		if h["AutoRemove"] != true || h["Memory"] != float64(2<<30) || h["MemorySwap"] != float64(2<<30) {
			t.Fatal(h)
		}
	}
	restarted, e := open(context.Background(), p.cfg, &engine{base: s.URL, client: s.Client()}, false)
	if e != nil {
		t.Fatal(e)
	}
	plans, e := restarted.Submit(context.Background(), launches(requests("next")))
	if e != nil || plans[0].Status != compute.NoCapacity {
		t.Fatal(plans, e)
	}
	d.mu.Lock()
	for id, c := range d.containers {
		c.State.Status = "exited"
		d.containers[id] = c
	}
	d.mu.Unlock()
	plans, e = restarted.Submit(context.Background(), launches(requests("next")))
	if e != nil || plans[0].Status != compute.Accepted {
		t.Fatal(plans, e)
	}
}
func TestUnknownCreateRetainsCharge(t *testing.T) {
	d := &daemon{containers: map[string]container{}, createError: true}
	p, s := testProvider(t, d)
	defer s.Close()
	ls := launches(requests("a", "b"))
	r, e := p.Submit(context.Background(), ls)
	if e != nil || r[0].Status != compute.Unknown {
		t.Fatal(r, e)
	}
	plans, e := p.Submit(context.Background(), launches(requests("c")))
	if e != nil || plans[0].Status != compute.NoCapacity {
		t.Fatal(plans, e)
	}
}
func TestRetriedActiveLaunchDoesNotRestart(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	ls := launches(requests("a"))
	r, _ := p.Submit(context.Background(), ls)
	if r[0].Status != compute.Accepted {
		t.Fatal(r)
	}
	d.mu.Lock()
	c := d.containers["a"]
	c.State.Status = "running"
	d.containers["a"] = c
	d.mu.Unlock()
	r, _ = p.Submit(context.Background(), ls)
	if r[0].Status != compute.Accepted || len(d.creates) != 1 {
		t.Fatal(r)
	}
	ls[0].Process.Args = []string{"different"}
	r, _ = p.Submit(context.Background(), ls)
	if r[0].Status != compute.Rejected {
		t.Fatal(r)
	}
}

func TestRoundingPreservesRequestedEnvironment(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	rs := requests("round")
	rs[0].CPUMillis = 1001
	ls := launches(rs)
	ls[0].Process.Env = map[string]string{"C2J_EXECUTION_CPU": "1001m", "PULSE_SCRATCH_DIR": "/custom", "TMPDIR": "/custom/tmp", "LITERAL": "$HOME"}
	ls[0].Process.Stdin = "private-capability"
	out, err := p.Submit(context.Background(), ls)
	if err != nil || out[0].Status != compute.Accepted {
		t.Fatal(out, err)
	}
	body := d.creates[0]
	if body["HostConfig"].(map[string]any)["NanoCpus"] != float64(1010*1000000) {
		t.Fatal(body)
	}
	seen := map[string]string{}
	for _, v := range body["Env"].([]any) {
		k, value, _ := strings.Cut(v.(string), "=")
		if _, exists := seen[k]; exists {
			t.Fatal("duplicate environment key", k)
		}
		seen[k] = value
	}
	for k, v := range ls[0].Process.Env {
		if seen[k] != v {
			t.Fatal(k, seen[k], v)
		}
	}
	if seen[compute.StdinEnv] != "" || body["OpenStdin"] != true || body["StdinOnce"] != true || body["HostConfig"].(map[string]any)["Binds"] != nil || body["Entrypoint"].([]any)[0] != "c2j" {
		t.Fatal("expected direct executor, native stdin, and no host mounts", body)
	}
}

func TestRemovalDuringAccounting(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			d := &daemon{containers: map[string]container{}}
			p, server := testProvider(t, d)
			defer server.Close()
			results, err := p.Submit(context.Background(), launches(requests("a")))
			if err != nil || results[0].Status != compute.Accepted {
				t.Fatal(results, err)
			}
			d.mu.Lock()
			d.inspectError = status
			d.mu.Unlock()
			results, err = p.Submit(context.Background(), launches(requests("b")))
			if status == http.StatusNotFound {
				if err != nil || results[0].Status != compute.Accepted {
					t.Fatal("removed container still consumes capacity", results, err)
				}
			} else if err == nil || results[0].Status != compute.Unavailable {
				t.Fatal("non-removal inspection errors must fail closed", results, err)
			}
		})
	}
}
