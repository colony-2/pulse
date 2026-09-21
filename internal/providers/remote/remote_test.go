package remote

import (
	"context"
	"encoding/json"
	"github.com/colony-2/cortex/pkg/compute"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWireAndPartialAcceptance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		var body map[string][]map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body["items"]) != 2 {
			t.Error(body)
		}
		for _, i := range body["items"] {
			if _, ok := i["plan_token"]; !ok {
				t.Error("missing token")
			}
			if _, ok := i["Plan"]; ok {
				t.Error("native plan leaked")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"launch_id":"a","status":"accepted","inspection_uri":"/v1/launches/a","refs":[]},{"launch_id":"b","status":"no_capacity","reason":"full"}]}`))
	}))
	defer server.Close()
	c, e := New(server.URL, nil, func() (string, error) { return "secret", nil }, true)
	if e != nil {
		t.Fatal(e)
	}
	ls := []compute.PreparedLaunch{}
	for _, id := range []string{"a", "b"} {
		ls = append(ls, compute.PreparedLaunch{LaunchID: id, Plan: compute.Plan{Token: id, ExpiresAt: time.Now().Add(time.Hour)}, Process: compute.Process{Command: []string{"c2j"}}})
	}
	out, e := c.Submit(context.Background(), ls)
	if e != nil || len(out) != 2 || out[0].Status != compute.Accepted || !out[1].Status.Fallback() {
		t.Fatal(out, e)
	}
}
func TestFailureClassification(t *testing.T) {
	for _, test := range []struct {
		code int
		body string
		want compute.Status
	}{{503, "", compute.Unknown}, {422, "", compute.Rejected}, {200, `{"results":[{"launch_id":"a","status":"accepted"}]}`, compute.Unknown}, {200, `{`, compute.Unknown}} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.code); w.Write([]byte(test.body)) }))
		c, _ := New(s.URL, nil, nil, true)
		out, _ := c.Submit(context.Background(), []compute.PreparedLaunch{{LaunchID: "a", Plan: compute.Plan{Token: "token"}, Process: compute.Process{Command: []string{"x"}}}})
		if len(out) != 1 || out[0].Status != test.want {
			t.Fatalf("%d: %+v", test.code, out)
		}
		s.Close()
	}
}
func TestRedirectDoesNotForwardToken(t *testing.T) {
	hit := false
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer dest.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, 307) }))
	defer s.Close()
	c, _ := New(s.URL, nil, func() (string, error) { return "token", nil }, true)
	_, e := c.Inspect(context.Background(), "id")
	if e == nil || hit {
		t.Fatal("redirect followed")
	}
}

func TestPreparationUnavailableAllowsFallback(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer s.Close()
	c, _ := New(s.URL, nil, nil, true)
	out, e := c.Prepare(context.Background(), []compute.Request{{LaunchID: "job", Allocation: compute.Allocation{CPUMillis: 1000, MemoryBytes: 1024, ScratchBytes: 1024, Platform: "linux/amd64", Image: "alpine:3"}, TimeoutSeconds: 60, Metadata: map[string]string{"job": "j"}}})
	if e == nil || len(out) != 1 || out[0].Status != compute.Unavailable {
		t.Fatal(out, e)
	}
}
