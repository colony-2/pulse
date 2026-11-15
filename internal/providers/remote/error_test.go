package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/colony-2/pulse/pkg/compute"
)

type errorTransport func(*http.Request) (*http.Response, error)

func (f errorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failedBody struct{ err error }

func (r failedBody) Read([]byte) (int, error) { return 0, r.err }

type countingBody struct{ remaining, read int }

func (r *countingBody) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

func TestErrorBodyPartialReadAndLimit(t *testing.T) {
	readErr := errors.New("provider connection closed mid-body")
	raw := "resource limit exceeded\noriginal \"quota\" response"
	oversized := &countingBody{remaining: maxResponse + 100}
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   string
		cause  error
	}{
		{"partial", io.MultiReader(strings.NewReader(raw), failedBody{readErr}), "provider returned HTTP 422: " + raw + "\n" + readErr.Error(), readErr},
		{"limit", oversized, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: errorTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusUnprocessableEntity, Body: io.NopCloser(tc.reader)}, nil
			})}
			c, err := New("https://fixture.invalid", client, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Submit(context.Background(), []compute.Launch{launch("a")})
			if err == nil || len(out) != 1 || out[0].Status != compute.Rejected || out[0].Reason != err.Error() {
				t.Fatal(out, err)
			}
			if tc.cause != nil && (!errors.Is(err, tc.cause) || err.Error() != tc.want) {
				t.Fatal(err)
			}
			if tc.name == "limit" {
				prefix := "provider returned HTTP 422: "
				suffix := fmt.Sprintf("\n[truncated after %d bytes]", maxResponse)
				text := err.Error()
				if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, suffix) || len(text) != len(prefix)+maxResponse+len(suffix) || oversized.read != maxResponse+1 {
					t.Fatal("error capture changed its bound", len(text), oversized.read)
				}
			}
		})
	}
}
