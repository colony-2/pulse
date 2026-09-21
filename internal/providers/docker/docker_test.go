package docker

import (
	"context"
	"encoding/json"
	"github.com/colony-2/cortex/pkg/compute"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type daemon struct {
	mu          sync.Mutex
	containers  map[string]container
	creates     []map[string]any
	startError  bool
	createError bool
}

func (d *daemon) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.47")
	enc := json.NewEncoder(w)
	switch {
	case path == "/version":
		enc.Encode(map[string]any{"ApiVersion": "1.47"})
	case path == "/info":
		enc.Encode(map[string]any{"ID": "test", "NCPU": 8, "MemTotal": int64(16 << 30), "OSType": "linux", "Architecture": "arm64", "MemoryLimit": true, "SwapLimit": true, "CpuCfsQuota": true})
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
		id := labels["cortex_launch_id"]
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
		enc.Encode(d.containers[id])
	default:
		w.WriteHeader(404)
	}
}
func testProvider(t *testing.T, d *daemon) (*Provider, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(d.serve))
	p, e := open(context.Background(), Config{CPUMillis: 2000, MemoryBytes: 4 << 30, MaxContainers: 2, Helper: "/helper"}, &engine{base: s.URL, client: s.Client()}, false)
	if e != nil {
		t.Fatal(e)
	}
	return p, s
}
func requests(ids ...string) []compute.Request {
	rs := []compute.Request{}
	for _, id := range ids {
		rs = append(rs, compute.Request{LaunchID: id, Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: "runner:1", Platform: "linux/arm64"}, TimeoutSeconds: 60, Metadata: map[string]string{"cortex_job_id": id}})
	}
	return rs
}
func launches(t *testing.T, p *Provider, rs []compute.Request) []compute.PreparedLaunch {
	t.Helper()
	plans, e := p.Prepare(context.Background(), rs)
	if e != nil {
		t.Fatal(e)
	}
	out := []compute.PreparedLaunch{}
	for _, r := range plans {
		if r.Status != compute.Prepared {
			t.Fatalf("not prepared: %+v", r)
		}
		out = append(out, compute.PreparedLaunch{LaunchID: r.LaunchID, Plan: *r.Plan, Process: compute.Process{Command: []string{"c2j"}, Args: []string{}, Env: map[string]string{}}})
	}
	return out
}
func TestConcurrentBatchesAndRestart(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	a := launches(t, p, requests("a", "b"))
	b := launches(t, p, requests("c", "d"))
	results := make(chan []compute.Submission, 2)
	for _, ls := range [][]compute.PreparedLaunch{a, b} {
		go func(ls []compute.PreparedLaunch) {
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
		if h["Memory"] != float64(2<<30) || h["MemorySwap"] != float64(2<<30) {
			t.Fatal(h)
		}
	}
	restarted, e := open(context.Background(), p.cfg, &engine{base: s.URL, client: s.Client()}, false)
	if e != nil {
		t.Fatal(e)
	}
	plans, e := restarted.Prepare(context.Background(), requests("next"))
	if e != nil || plans[0].Status != compute.NoCapacity {
		t.Fatal(plans, e)
	}
	d.mu.Lock()
	for id, c := range d.containers {
		c.State.Status = "exited"
		d.containers[id] = c
	}
	d.mu.Unlock()
	plans, e = restarted.Prepare(context.Background(), requests("next"))
	if e != nil || plans[0].Status != compute.Prepared {
		t.Fatal(plans, e)
	}
}
func TestUnknownCreateRetainsCharge(t *testing.T) {
	d := &daemon{containers: map[string]container{}, createError: true}
	p, s := testProvider(t, d)
	defer s.Close()
	ls := launches(t, p, requests("a", "b"))
	r, e := p.Submit(context.Background(), ls)
	if e != nil || r[0].Status != compute.Unknown {
		t.Fatal(r, e)
	}
	plans, e := p.Prepare(context.Background(), requests("c"))
	if e != nil || plans[0].Status != compute.NoCapacity {
		t.Fatal(plans, e)
	}
}
func TestRetriedLaunchDoesNotRestart(t *testing.T) {
	d := &daemon{containers: map[string]container{}}
	p, s := testProvider(t, d)
	defer s.Close()
	ls := launches(t, p, requests("a"))
	r, _ := p.Submit(context.Background(), ls)
	if r[0].Status != compute.Accepted {
		t.Fatal(r)
	}
	d.mu.Lock()
	c := d.containers["a"]
	c.State.Status = "exited"
	d.containers["a"] = c
	d.mu.Unlock()
	ls[0].Plan.ExpiresAt = time.Now().Add(-time.Hour)
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
