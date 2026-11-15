package controller

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
	"github.com/colony-2/pulse/pkg/providerplugin"
)

type reviewReasonProvider struct{}

func (reviewReasonProvider) Submit(_ context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	return []compute.Submission{
		{LaunchID: ls[0].LaunchID, Status: compute.NoCapacity, Reason: "slots occupied: reservation ledger contains launch-a"},
		{LaunchID: ls[0].LaunchID, Status: compute.NoCapacity, Reason: "tenant memory limit: requested=2147483648 remaining=1073741824"},
	}, nil
}
func (reviewReasonProvider) List(context.Context, compute.ListRequest) (compute.ListResponse, error) {
	panic("unexpected listing")
}
func TestReviewReasonPluginProcess(t *testing.T) {
	if os.Getenv("PULSE_PLUGIN_COOKIE") == "" {
		return
	}
	providerplugin.Serve(func(context.Context, providerplugin.Init) (compute.Provider, error) {
		return reviewReasonProvider{}, nil
	})
	os.Exit(0)
}
func TestReviewPluginOriginalReasons(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	native, err := providerplugin.Start(t.Context(), providerplugin.ProcessConfig{Command: executable, Args: []string{"-test.run=^TestReviewReasonPluginProcess$"}, Name: "reason-fixture", BaseDir: t.TempDir(), ConfigYAML: []byte("{}"), StartTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	c, lease, buf := transportDiagnosticFixture(t, native, "plugin")
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
	for _, raw := range []string{"slots occupied: reservation ledger contains launch-a", "tenant memory limit: requested=2147483648 remaining=1073741824"} {
		if !strings.Contains(buf.String(), raw) {
			t.Errorf("original provider Reason discarded before correlated log: missing=%q record=%s", raw, buf.String())
		}
	}
}
