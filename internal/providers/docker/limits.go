package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"
)

var errLimitsDiscarded = errors.New("Docker discarded probe resource limits")

// checkLimits tests the daemon rather than the controller's cgroup namespace.
// Some nested daemons advertise limits in /info but cannot apply them. Use a
// disposable Alpine container, with no host mounts or installed helper.
func (p *Provider) checkLimits(ctx context.Context) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := "pulse-limits-probe-" + hex.EncodeToString(nonce[:])
	const probeImage = "alpine:3.21"
	var image struct {
		ID string `json:"Id"`
	}
	imagePath := "/images/" + url.PathEscape(probeImage) + "/json"
	err := p.e.call(ctx, "GET", imagePath, nil, &image)
	var api *apiError
	if errors.As(err, &api) && api.code == 404 {
		if err = p.e.pull(ctx, probeImage, p.platform); err == nil {
			err = p.e.call(ctx, "GET", imagePath, nil, &image)
		}
	}
	if err != nil {
		return probeDiagnostic("resolve probe image", err)
	}
	if image.ID == "" {
		return fmt.Errorf("Docker probe image has no ID")
	}
	containers := []string{}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		paths := []string{}
		for _, container := range containers {
			paths = append(paths, "/containers/"+container+"?force=1")
		}
		for _, path := range paths {
			if err := p.e.call(cleanup, "DELETE", path, nil, nil); err != nil {
				var api *apiError
				if !errors.As(err, &api) || api.code != 404 {
					slog.Warn("Docker startup probe cleanup failed", "resource", path, "error", err)
				}
			}
		}
	}()
	start := func(limited bool) error {
		containerName := fmt.Sprintf("%s-%t", name, limited)
		containers = append(containers, containerName)
		host := map[string]any{"NetworkMode": "none"}
		if limited {
			host["NanoCpus"] = int64(100000000)
			host["Memory"] = int64(32 << 20)
			host["MemorySwap"] = int64(32 << 20)
		}
		body := map[string]any{"Image": image.ID, "Entrypoint": []string{"/bin/true"}, "HostConfig": host}
		var created struct {
			ID string `json:"Id"`
		}
		if err := p.e.call(ctx, "POST", "/containers/create?name="+containerName, body, &created); err != nil {
			return probeDiagnostic("create container", err)
		}
		if created.ID == "" {
			return fmt.Errorf("Docker probe create returned no ID")
		}
		// Docker can silently discard unsupported limits at create time.
		if limited {
			var inspected container
			if err := p.e.call(ctx, "GET", "/containers/"+created.ID+"/json", nil, &inspected); err != nil {
				return probeDiagnostic("inspect container", err)
			}
			if inspected.HostConfig.NanoCPUs != 100000000 || inspected.HostConfig.Memory != 32<<20 || inspected.HostConfig.MemorySwap != 32<<20 {
				return errLimitsDiscarded
			}
		}
		if err := p.e.call(ctx, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
			return probeDiagnostic("start container", err)
		}
		return nil
	}
	if err := start(true); err != nil {
		var api *apiError
		if !errors.Is(err, errLimitsDiscarded) && (!errors.As(err, &api) || (api.code != 400 && api.code < 500)) {
			return fmt.Errorf("Docker resource-limit probe: %w", err)
		}
		slog.Info("Docker resource-limit probe failed; checking startup without limits", "error", err)
		// Confirm the daemon can actually run unconstrained containers. An unrelated
		// failure (bad image, permissions, networking, etc.) remains a startup error.
		if unlimitedErr := start(false); unlimitedErr != nil {
			return fmt.Errorf("Docker startup probes failed (platform %s): %w", p.platform,
				errors.Join(fmt.Errorf("with resource limits: %w", err), fmt.Errorf("without resource limits: %w", unlimitedErr)))
		}
		p.disableLimits("Docker startup probe succeeded only without resource limits: " + err.Error())
	}
	return nil
}

// Only synthetic startup probes expose daemon details. Job submission errors
// retain their redacted formatting because Docker can echo sensitive job input.
func probeDiagnostic(stage string, err error) error {
	var api *apiError
	if errors.As(err, &api) && api.message != "" {
		return fmt.Errorf("Docker probe %s: %w: daemon message %q", stage, err, api.message)
	}
	return fmt.Errorf("Docker probe %s: %w", stage, err)
}
