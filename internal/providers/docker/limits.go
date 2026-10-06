package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errLimitsDiscarded = errors.New("Docker discarded probe resource limits")

// checkLimits tests the daemon rather than the controller's cgroup namespace.
// Some nested daemons advertise limits in /info but cannot apply them. A tiny
// temporary scratch image and the already installed static helper let us probe
// startup without pulling an image or executing any user job.
func (p *Provider) checkLimits(ctx context.Context) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := "pulse-limits-probe-" + hex.EncodeToString(nonce[:])
	image := name + ":probe"
	containers := []string{}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		paths := []string{}
		for _, container := range containers {
			paths = append(paths, "/containers/"+container+"?force=1")
		}
		paths = append(paths, "/images/"+image)
		for _, path := range paths {
			if err := p.e.call(cleanup, "DELETE", path, nil, nil); err != nil {
				var api *apiError
				if !errors.As(err, &api) || api.code != 404 {
					slog.Warn("Docker startup probe cleanup failed", "resource", path, "error", err)
				}
			}
		}
	}()
	if err := p.e.importProbeImage(ctx, name, p.platform); err != nil {
		return fmt.Errorf("Docker resource-limit probe image: %w", err)
	}
	start := func(limited bool) error {
		containerName := fmt.Sprintf("%s-%t", name, limited)
		containers = append(containers, containerName)
		host := map[string]any{"Binds": []string{p.cfg.Helper + ":/__pulse/exec:ro"}, "NetworkMode": "none"}
		if limited {
			host["NanoCpus"] = int64(100000000)
			host["Memory"] = int64(32 << 20)
			host["MemorySwap"] = int64(32 << 20)
		}
		body := map[string]any{"Image": image, "Entrypoint": []string{"/__pulse/exec"}, "HostConfig": host}
		var created struct {
			ID string `json:"Id"`
		}
		if err := p.e.call(ctx, "POST", "/containers/create?name="+containerName, body, &created); err != nil {
			return err
		}
		if created.ID == "" {
			return fmt.Errorf("Docker probe create returned no ID")
		}
		// Docker can silently discard unsupported limits at create time.
		if limited {
			var inspected container
			if err := p.e.call(ctx, "GET", "/containers/"+created.ID+"/json", nil, &inspected); err != nil {
				return err
			}
			if inspected.HostConfig.NanoCPUs != 100000000 || inspected.HostConfig.Memory != 32<<20 || inspected.HostConfig.MemorySwap != 32<<20 {
				return errLimitsDiscarded
			}
		}
		return p.e.call(ctx, "POST", "/containers/"+created.ID+"/start", nil, nil)
	}
	if err := start(true); err != nil {
		var api *apiError
		if !errors.Is(err, errLimitsDiscarded) && (!errors.As(err, &api) || !limitError(api.message)) {
			return fmt.Errorf("Docker resource-limit probe: %w", err)
		}
		// Confirm the daemon can actually run unconstrained containers. An unrelated
		// failure (bad helper, permissions, networking, etc.) remains a startup error.
		if err := start(false); err != nil {
			return fmt.Errorf("Docker probe without resource limits: %w", err)
		}
		p.disableLimits("Docker startup probe could not apply cgroup resource limits")
	}
	return nil
}

func limitError(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "cgroup") ||
		strings.Contains(message, "memory limit") ||
		strings.Contains(message, "swap limit") ||
		strings.Contains(message, "cpu quota") ||
		strings.Contains(message, "cpu cfs") ||
		strings.Contains(message, "nanocpus")
}

func (e *engine) importProbeImage(ctx context.Context, name, platform string) error {
	var archive bytes.Buffer
	if err := tar.NewWriter(&archive).Close(); err != nil {
		return err
	}
	query := url.Values{"fromSrc": {"-"}, "repo": {name}, "tag": {"probe"}, "platform": {platform}}
	req, err := http.NewRequestWithContext(ctx, "POST", e.base+e.version+"/images/create?"+query.Encode(), &archive)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &apiError{code: resp.StatusCode}
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	for {
		var event struct{ Error string }
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if event.Error != "" {
			return fmt.Errorf("Docker probe image import failed")
		}
	}
}
