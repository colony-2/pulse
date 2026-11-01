// Package plugintest supplies reusable tests for a disposable provider scope.
package plugintest

import (
	"context"
	"github.com/colony-2/pulse/pkg/compute"
	"testing"
	"time"
)

// Check submits a caller-supplied valid launch to a disposable backend and checks
// acceptance and exact filtered active listing. The fixture must keep it active
// for this test and arrange native workload cleanup after the test.
func Check(t *testing.T, p compute.Provider, l compute.Launch) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := l.Validate(); e != nil {
		t.Fatal("invalid conformance launch")
	}
	out, e := p.Submit(ctx, []compute.Launch{l})
	if e != nil || len(out) != 1 || out[0].LaunchID != l.LaunchID || out[0].Status != compute.Accepted {
		t.Fatal("provider did not accept conformance launch", e)
	}
	q := compute.ListRequest{PageSize: 1, LaunchID: l.LaunchID}
	seen := map[string]bool{}
	found := false
	for pages := 0; pages < 100; pages++ {
		page, e := p.List(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		if page.Items == nil || len(page.Items) > 1 {
			t.Fatal("invalid list page")
		}
		for _, i := range page.Items {
			if i.Validate() != nil || i.LaunchID != l.LaunchID || seen[i.ID] {
				t.Fatal("invalid filtered instance")
			}
			for k, v := range l.Metadata {
				if i.Metadata[k] != v {
					t.Fatal("metadata changed")
				}
			}
			seen[i.ID] = true
			found = true
		}
		if page.NextPageToken == "" {
			if !found {
				t.Fatal("accepted instance absent")
			}
			return
		}
		if page.NextPageToken == q.PageToken {
			t.Fatal("cursor did not advance")
		}
		q.PageToken = page.NextPageToken
	}
	t.Fatal("listing did not terminate")
}
