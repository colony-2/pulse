package supervisor

import (
	"context"
	"testing"
	"time"
)

func TestExitAndTimeout(t *testing.T) {
	if n := Run(context.Background(), []string{"/bin/sh", "-c", "exit 7"}, time.Second, time.Time{}); n != 7 {
		t.Fatal(n)
	}
	start := time.Now()
	if n := Run(context.Background(), []string{"/bin/sh", "-c", "sleep 30"}, 30*time.Millisecond, time.Time{}); n != 124 {
		t.Fatal(n)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not terminate")
	}
	if n := Run(context.Background(), []string{"/bin/true"}, time.Second, time.Now().Add(-time.Second)); n != 125 {
		t.Fatal(n)
	}
}
