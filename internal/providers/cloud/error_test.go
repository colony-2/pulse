package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/colony-2/pulse/pkg/compute"
)

func TestCloudAdmissionErrorRetainsBody(t *testing.T) {
	const raw = "{\"code\":\"RESOURCE_EXHAUSTED\",\"message\":\"regional memory quota exceeded\"}\noriginal \"quota\" response"
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(raw))
	}))
	defer s.Close()
	p, err := New(Config{Kind: "cloudrun", Project: "fixture", Region: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	p.BaseURL, p.HTTP = s.URL, s.Client()
	p.accessToken = func(context.Context) (string, error) { return "fixture-token", nil }
	out, err := p.Submit(context.Background(), []compute.Launch{launch(request("a"))})
	if err != nil || len(out) != 1 || out[0].Status != compute.NoCapacity || out[0].Reason != "cloud API HTTP 429: "+raw || calls != 1 {
		t.Fatal(out, err, calls)
	}
}

func TestCloudPreparationErrorRetainsCause(t *testing.T) {
	p, err := New(Config{Kind: "cloudrun", Project: "fixture", Region: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	const raw = "credential refresh failed: identity endpoint returned 503\noriginal cause"
	p.accessToken = func(context.Context) (string, error) { return "", errors.New(raw) }
	out, err := p.Submit(context.Background(), []compute.Launch{launch(request("a"))})
	if err != nil || len(out) != 1 || out[0].Status != compute.Unavailable || out[0].Reason != "cloud authentication unavailable: "+raw {
		t.Fatal(out, err)
	}
}
