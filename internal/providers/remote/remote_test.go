package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/colony-2/cortex/pkg/compute"
)

func launch(id string) compute.Launch {
	deadline := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return compute.Launch{Request: compute.Request{LaunchID: id, Allocation: compute.Allocation{CPUMillis: 1500, MemoryBytes: 1 << 30, ScratchBytes: 1 << 30, Image: "alpine:3", Platform: "linux/amd64"}, TimeoutSeconds: 60, StartBefore: &deadline, Metadata: map[string]string{"cortex_job_id": "job-" + id}}, Process: compute.Process{Command: []string{"c2j"}, Args: []string{"run", "--job", "job-" + id}, Env: map[string]string{"C2J_EXECUTION_CPU": "1500m", "LITERAL": "${allocation.cpu} $HOME"}}}
}

func TestWireAndPartialAcceptance(t *testing.T) {
	launches := []compute.Launch{launch("a"), launch("b")}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/v1/submit" {
			t.Error(r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		var body struct {
			Items []compute.Launch `json:"items"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if !reflect.DeepEqual(body.Items, launches) {
			t.Errorf("wire request changed: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"launch_id":"a","status":"accepted"},{"launch_id":"b","status":"no_capacity","reason":"full"}]}`))
	}))
	defer server.Close()
	c, e := New(server.URL, nil, func() (string, error) { return "secret", nil }, true)
	if e != nil {
		t.Fatal(e)
	}
	out, e := c.Submit(context.Background(), launches)
	if e != nil || calls != 1 || len(out) != 2 || out[0].Status != compute.Accepted || !out[1].Status.Fallback() {
		t.Fatal(out, e, calls)
	}
}

func TestFailureClassification(t *testing.T) {
	for _, test := range []struct {
		code int
		body string
		want compute.Status
	}{
		{503, "", compute.Unknown}, {429, "", compute.Unknown}, {422, "", compute.Rejected},
		{200, `{"results":[{"launch_id":"a","status":"accepted"}]}`, compute.Accepted},
		{200, `{"results":[{"launch_id":"a","status":"no_capacity"}]}`, compute.Unknown},
		{200, `{"results":[{"launch_id":"a","status":"unknown"}]}`, compute.Unknown},
		{200, `{"results":[{"launch_id":"a","status":"prepared","reason":"legacy"}]}`, compute.Unknown},
		{200, `{`, compute.Unknown},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.code); w.Write([]byte(test.body)) }))
		c, _ := New(s.URL, nil, nil, true)
		out, _ := c.Submit(context.Background(), []compute.Launch{launch("a")})
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
	_, e := c.List(context.Background(), compute.ListRequest{})
	if e == nil || hit {
		t.Fatal("redirect followed")
	}
}

func TestInvalidBatchMakesNoRequest(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
	defer s.Close()
	c, _ := New(s.URL, nil, nil, true)
	invalid := launch("b")
	invalid.Process.Command = nil
	for _, batch := range [][]compute.Launch{nil, {launch("a"), launch("a")}, {launch("a"), invalid}} {
		if _, err := c.Submit(context.Background(), batch); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid batch reached provider", calls)
	}
}

func TestListWireAndValidation(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/prefix/v1/launches" || r.URL.Query().Get("launch_id") != "launch" || r.URL.Query().Get("page_token") != "opaque+/=?" {
				t.Error(r.Method, r.URL)
			}
			state := "queued"
			if invalid {
				state = "succeeded"
			}
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "instance", "launch_id": "launch", "state": state, "metadata": map[string]string{"cortex_launch_id": "launch"}, "refs": []string{}}}, "next_page_token": "next"})
		}))
		c, _ := New(server.URL+"/prefix", nil, nil, true)
		page, err := c.List(context.Background(), compute.ListRequest{LaunchID: "launch", PageToken: "opaque+/=?"})
		if invalid && err == nil {
			t.Fatal("terminal instance accepted")
		}
		if !invalid && (err != nil || len(page.Items) != 1 || page.NextPageToken != "next") {
			t.Fatal(page, err)
		}
		server.Close()
	}
}
