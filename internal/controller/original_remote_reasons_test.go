package controller

import (
	"encoding/json"
	"github.com/colony-2/pulse/internal/providers/remote"
	"github.com/colony-2/pulse/pkg/compute"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewRemoteOriginalReasons(t *testing.T) {
	const raw = "native allocation limit: requested=2147483648 remaining=1073741824"
	for _, scenario := range []string{"invalid_status", "unrelated_result"} {
		t.Run(scenario, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Items []struct {
						LaunchID string `json:"launch_id"`
					} `json:"items"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				status := compute.NoCapacity
				if scenario == "invalid_status" {
					status = "provider_specific_status"
				}
				out := []compute.Submission{{LaunchID: req.Items[0].LaunchID, Status: status, Reason: raw}}
				if scenario == "unrelated_result" {
					out = append(out, compute.Submission{LaunchID: "unrelated-id", Status: compute.NoCapacity, Reason: "belongs to another launch"})
				}
				json.NewEncoder(w).Encode(map[string]any{"results": out})
			}))
			defer s.Close()
			native, err := remote.New(s.URL, s.Client(), nil, true)
			if err != nil {
				t.Fatal(err)
			}
			c, lease, buf := transportDiagnosticFixture(t, native, "remote")
			if err := c.Once(t.Context()); err != nil {
				t.Fatal(err)
			}
			d := decodeLaunch(t, buf)
			t.Logf("original provider observations: %s", strings.TrimSpace(buf.String()))
			if len(d.Attempts) != 1 || d.Attempts[0].SubmissionErrorScope != "batch" {
				t.Fatal(d.Attempts)
			}
			if d.Admission != "uncertain" || lease.calls != 0 {
				t.Fatal("normalization/ownership changed", d, lease.calls)
			}
			if !strings.Contains(buf.String(), raw) {
				t.Errorf("matching original provider Reason discarded before correlated log: missing=%q record=%s", raw, buf.String())
			}
			if strings.Contains(buf.String(), "belongs to another launch") {
				t.Error("unrelated reason attributed to current job")
			}
		})
	}
}
