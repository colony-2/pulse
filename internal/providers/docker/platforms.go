package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"
)

type platformProbe struct {
	Status string
	Stage  string
	Err    error
}

// NativePlatform reports the daemon's platform, independently of the controller.
func (p *Provider) NativePlatform() string { return p.platform }

// SupportedPlatforms returns the native platform and verified execution platforms.
// Discovery finishes before the provider is returned to callers.
func (p *Provider) SupportedPlatforms() []string {
	platforms := []string{p.platform}
	for platform, probe := range p.platformProbes {
		if probe.Status == "supported" {
			platforms = append(platforms, platform)
		}
	}
	slices.Sort(platforms)
	return platforms
}

func (p *Provider) supportsPlatform(platform string) bool {
	return platform == p.platform || p.platformProbes[platform].Status == "supported"
}

func (p *Provider) platformReason(platform string) string {
	if probe, ok := p.platformProbes[platform]; ok {
		return fmt.Sprintf("Docker platform %s not enabled: discovery %s at %s", platform, probe.Status, probe.Stage)
	}
	return "Docker platform not in detected supported set"
}

func (p *Provider) discoverPlatforms(ctx context.Context) error {
	p.platformProbes = map[string]platformProbe{}
	for _, platform := range []string{"linux/arm64", "linux/amd64"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if platform == p.platform {
			continue
		}
		if !p.e.platformInspection() {
			p.platformProbes[platform] = platformProbe{Status: "skipped", Stage: "API version"}
			slog.Info("Docker platform discovery skipped", "platform", platform, "reason", "requires Engine API 1.49 or later")
			continue
		}
		// Optional discovery cannot consume the entire initialization deadline.
		budget := 30 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			budget = min(budget, time.Until(deadline)/2)
		}
		probeCtx, cancel := context.WithTimeout(ctx, budget)
		result := p.probePlatform(probeCtx, platform)
		cancel()
		p.platformProbes[platform] = result
		if err := ctx.Err(); err != nil {
			return err
		}
		if result.Status == "supported" {
			slog.Info("Docker platform execution verified", "platform", platform)
		} else {
			slog.Warn("Docker platform not enabled", "platform", platform, "discovery", result.Status, "stage", result.Stage, "error", result.Err)
		}
	}
	return ctx.Err()
}

func (p *Provider) probePlatform(ctx context.Context, platform string) platformProbe {
	failure := func(stage string, err error) platformProbe {
		status := "inconclusive"
		// Start errors unrelated to architecture (permissions, daemon failure,
		// etc.) must not be reported as proof that emulation is unavailable.
		var api *apiError
		if stage == "start" && errors.As(err, &api) && strings.Contains(strings.ToLower(api.message), "exec format error") {
			status = "execution_failed"
		}
		return platformProbe{Status: status, Stage: stage, Err: probeDiagnostic(stage, err)}
	}
	image, err := p.e.resolveImage(ctx, probeImageForPlatform(platform), platform)
	if err != nil {
		return failure("resolve image", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return failure("name", err)
	}
	name := "pulse-platform-probe-" + hex.EncodeToString(nonce[:])
	// Register cleanup by name before create: even an ambiguous create outcome
	// may have left a container behind. Probes never contain job input.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.e.call(cleanup, "DELETE", "/containers/"+name+"?force=1", nil, nil); err != nil && !apiStatus(err, 404) {
			slog.Warn("Docker platform probe cleanup failed", "container", name, "error", err)
		}
	}()
	body := map[string]any{
		"Image": image.ID, "Entrypoint": []string{probeCommand}, "Cmd": []string{},
		"HostConfig": map[string]any{"NetworkMode": "none", "RestartPolicy": map[string]string{"Name": "no"}, "LogConfig": map[string]string{"Type": "none"}},
	}
	var created struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	if err := p.e.call(ctx, "POST", containerCreatePath(name, platform), body, &created); err != nil {
		return failure("create", err)
	}
	if created.ID == "" || len(created.Warnings) != 0 {
		return failure("create", fmt.Errorf("probe creation could not be verified"))
	}
	path := "/containers/" + url.PathEscape(created.ID)
	execution, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.e.call(execution, "POST", path+"/start", nil, nil); err != nil {
		return failure("start", err)
	}
	var waited struct {
		StatusCode *int
		Error      *struct{ Message string }
	}
	if err := p.e.call(execution, "POST", path+"/wait?condition=not-running", nil, &waited); err != nil {
		return failure("wait", err)
	}
	if waited.Error != nil || waited.StatusCode == nil {
		return failure("wait", fmt.Errorf("probe completion could not be verified"))
	}
	if *waited.StatusCode != 0 {
		return platformProbe{Status: "execution_failed", Stage: "wait", Err: fmt.Errorf("probe exited with code %d", *waited.StatusCode)}
	}
	return platformProbe{Status: "supported", Stage: "wait"}
}
