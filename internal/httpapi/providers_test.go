package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	provider "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/config"
)

type catalogProvider struct {
	fakeProvider
	capabilities provider.Capabilities
	err          error
}

func (p catalogProvider) GetCapabilities(context.Context) (provider.Capabilities, error) {
	return p.capabilities, p.err
}

func TestProviderDiscoveryInventory(t *testing.T) {
	const hiddenError = "private-provider-error"
	backends := map[string]provider.Provider{
		"worker": catalogProvider{capabilities: provider.Capabilities{BasesComplete: true, Bases: []provider.BaseCapabilities{
			{Name: "ubuntu-26.04-amd64"},
			{Name: "ubuntu-24.04-arm64", Capabilities: []string{"docker", "kvm"}},
			{Name: "macos-15-arm64"},
		}}},
		"offline": catalogProvider{err: sdk.ErrUnavailable},
		"legacy":  catalogProvider{},
		"broken":  catalogProvider{err: errors.New(hiddenError)},
		"invalid": catalogProvider{capabilities: provider.Capabilities{Bases: []provider.BaseCapabilities{{Name: "ubuntu*"}}}},
	}
	a := newAPI(t, backends)
	a.Controller.Config.ExcludeBases = []string{"ubuntu*amd64"}
	for name := range backends {
		a.Controller.Config.Providers[name] = config.Provider{Type: "reverse"}
	}
	h := a.Handler()
	response := get(h, "/providers")
	var list struct{ Items []providerDetails }
	if err := json.Unmarshal(response.Body.Bytes(), &list); response.Code != http.StatusOK || err != nil || strings.Contains(response.Body.String(), hiddenError) {
		t.Fatalf("provider inventory HTTP %d: %s, %v", response.Code, response.Body, err)
	}
	empty := []provider.BaseCapabilities{}
	wantWorker := providerDetails{
		Name: "worker", Type: "reverse", Discovery: "known", BasesComplete: true,
		Bases: []provider.BaseCapabilities{
			{Name: "ubuntu-24.04-arm64", Capabilities: []string{"docker", "kvm"}},
			{Name: "macos-15-arm64", Capabilities: []string{}},
		},
	}
	want := []providerDetails{
		{Name: "broken", Type: "reverse", Discovery: "error", Bases: empty},
		{Name: "invalid", Type: "reverse", Discovery: "invalid", Bases: empty},
		{Name: "legacy", Type: "reverse", Discovery: "unknown", Bases: empty},
		{Name: "offline", Type: "reverse", Discovery: "unavailable", Bases: empty},
		wantWorker,
	}
	if !reflect.DeepEqual(want, list.Items) {
		t.Fatalf("unexpected provider inventory: %+v", list.Items)
	}

	response = get(h, "/providers/worker/capabilities")
	var detail providerDetails
	if err := json.Unmarshal(response.Body.Bytes(), &detail); response.Code != http.StatusOK || err != nil || !reflect.DeepEqual(detail, wantWorker) {
		t.Fatalf("provider details HTTP %d: %s, %v", response.Code, response.Body, err)
	}
	if response := get(h, "/providers/missing/capabilities"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown provider HTTP %d: %s", response.Code, response.Body)
	}
}
