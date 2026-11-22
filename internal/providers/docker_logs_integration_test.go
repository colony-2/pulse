package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/c2j"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/internal/controller"
	"github.com/colony-2/pulse/internal/httpapi"
	"github.com/colony-2/pulse/internal/launchlogs"
	"github.com/colony-2/pulse/internal/providers"
	"github.com/colony-2/pulse/internal/scheduler"
)

func runDockerRetainedOnce(t *testing.T, ctx context.Context, jobDB, socket string) ([]byte, http.Handler, error) {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
log_directory: logs
providers:
  docker:
    type: docker
    socket: %q
    dependency_cache: {enabled: false}
targets:
  - jobdb: %q
    launch_services: [{name: docker, priority: 1}]
`, socket, jobDB)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseDir = t.TempDir()
	backends, closeAll, err := providers.Build(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeAll)
	jobdb, err := c2j.NewEmbedded([]c2j.Connection{{URI: jobDB}})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	// Leave the scheduler listener unset so the REST read proves native retention.
	c := &controller.Controller{Config: cfg, Providers: backends, Lister: jobdb, Claimer: jobdb,
		Scheduler: scheduler.New(cfg.Cool, cfg.Call), Log: slog.New(slog.NewJSONHandler(&output, nil))}
	rest, err := httpapi.New(c, "test")
	if err != nil {
		t.Fatal(err)
	}
	err = c.Once(ctx)
	return output.Bytes(), rest.Handler(), err
}

func waitForDockerRetainedLogs(t *testing.T, ctx context.Context, handler http.Handler, launchID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	path := "/providers/docker/launches/" + url.PathEscape(launchID) + "/logs"
	query := url.Values{"page_size": {"2"}}
	var workerOutput bytes.Buffer
	for {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, path+"?"+query.Encode(), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("Docker log API HTTP %d: %s", response.Code, response.Body)
		}
		var page launchlogs.Page
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.NextPageToken == "" {
			t.Fatalf("invalid Docker log page: %v: %s", err, response.Body)
		}
		for _, record := range page.Records {
			if record.Source == api.LogSourceStdout || record.Source == api.LogSourceStderr {
				workerOutput.Write(record.Data)
			}
		}
		if page.Finalized && !page.HasMore {
			if page.Incomplete || !bytes.Contains(workerOutput.Bytes(), []byte("done docker-executor")) {
				t.Fatalf("incomplete Docker launch output: incomplete=%t output=%q", page.Incomplete, workerOutput.Bytes())
			}
			return
		}
		query.Set("page_token", page.NextPageToken)
		if page.HasMore {
			continue
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Docker log archive did not finalize: %q", workerOutput.Bytes())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
