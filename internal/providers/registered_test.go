package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/providers"
	"github.com/colony-2/pulse/pkg/providerregistry"
)

var registrationSequence atomic.Int64

type externalProvider struct{ received []provider.Launch }

func (p *externalProvider) Submit(_ context.Context, ls []provider.Launch) ([]provider.Submission, error) {
	p.received = ls
	out := make([]provider.Submission, len(ls))
	for i, l := range ls {
		out[i] = provider.Submission{LaunchID: l.LaunchID, Status: provider.Accepted}
	}
	return out, nil
}
func (*externalProvider) List(context.Context, provider.ListRequest) (provider.ListResponse, error) {
	return provider.ListResponse{Items: []provider.Instance{}}, nil
}

func TestRegisteredGoProviderReceivesAPIWithoutNativeTranslation(t *testing.T) {
	p := &externalProvider{}
	closed := false
	kind := fmt.Sprintf("test-go-provider-%d", registrationSequence.Add(1))
	err := providerregistry.Register(kind, func(ctx context.Context, o providerregistry.Options) (provider.Provider, func(), error) {
		var settings struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(o.Config, &settings); err != nil || settings.Token != "private-setting" || o.Name != "custom" || o.BaseDir == "" || o.Env["EXTRA"] != "literal" {
			t.Error("incorrect provider options")
		}
		o.Env["EXTRA"] = "changed"
		return p, func() { closed = true }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(`defaults:
  base: custom-*
  env: {EXTRA: literal}
providers:
  custom:
    type: ` + kind + `
    config: {token: private-setting}
targets: [{jobdb: 'https://db/tenant', launch_services: [{name: custom, priority: 1}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	public, err := cfg.Public()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(public)
	if strings.Contains(string(raw), "private-setting") {
		t.Fatal("factory settings exposed")
	}
	instances, closeAll, err := providers.Build(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if instances["custom"] != p || cfg.Defaults.Env["EXTRA"] != "literal" {
		t.Fatal("provider wrapped or shared config mutated")
	}
	launch := provider.Launch{LaunchID: "external", Allocation: cfg.Allocation, Job: provider.JobRef{JobDB: "https://db/tenant", InstanceID: "db", TenantID: "tenant", JobID: "job"}, Lease: "opaque"}
	launch.Capabilities = []string{"custom.feature"}
	out, err := instances["custom"].Submit(t.Context(), []provider.Launch{launch})
	if err != nil || out[0].Status != provider.Accepted || p.received[0].Base != "custom-*" || p.received[0].Lease != "opaque" || p.received[0].Capabilities[0] != "custom.feature" {
		t.Fatal("external assignment altered")
	}
	closeAll()
	if !closed {
		t.Fatal("cleanup not called")
	}
}
func TestFailedGoProviderConstructionCleansUp(t *testing.T) {
	cleaned := false
	kind := fmt.Sprintf("test-go-failed-%d", registrationSequence.Add(1))
	err := providerregistry.Register(kind, func(context.Context, providerregistry.Options) (provider.Provider, func(), error) {
		return nil, func() { cleaned = true }, errors.New("private-detail")
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte("providers: {custom: {type: " + kind + "}}\ntargets: [{jobdb: 'https://db/tenant', launch_services: [{name: custom, priority: 1}]}]"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = providers.Build(t.Context(), cfg)
	if err == nil || !cleaned || strings.Contains(err.Error(), "private-detail") {
		t.Fatal("unsafe failed initialization", err)
	}
}
