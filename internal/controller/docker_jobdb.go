package controller

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"strings"

	"github.com/colony-2/pulse/pkg/compute"
)

// Discovery and claiming use the controller's URL. Only the command handed to
// a local Docker worker needs the container-to-host address. Adapt at provider
// selection so a cloud/remote fallback still receives the original command.
func dockerJobDBEndpoint(kind string, provider compute.Provider, endpoint string, log *slog.Logger) compute.Provider {
	if kind != "docker" {
		return provider
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return provider
	}
	host := u.Hostname()
	if !strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") && !net.ParseIP(host).IsLoopback() {
		return provider
	}
	port := u.Port()
	u.Host = "host.docker.internal"
	if port != "" {
		u.Host = net.JoinHostPort(u.Host, port)
	}
	return &dockerJobDBProvider{Provider: provider, source: endpoint, destination: u.String(), log: log}
}

type dockerJobDBProvider struct {
	compute.Provider
	source, destination string
	log                 *slog.Logger
}

func (p *dockerJobDBProvider) Submit(ctx context.Context, launches []compute.Launch) ([]compute.Submission, error) {
	adapted := append([]compute.Launch(nil), launches...)
	changed := false
	for i, launch := range launches {
		for j := 0; j+1 < len(launch.Process.Args); j++ {
			if launch.Process.Args[j] == "--jobdb" && launch.Process.Args[j+1] == p.source {
				adapted[i].Process.Args = append([]string(nil), launch.Process.Args...)
				adapted[i].Process.Args[j+1] = p.destination
				changed = true
				break
			}
		}
	}
	if changed {
		p.log.Info("using host JobDB address for Docker workers", "controller_jobdb", p.source, "worker_jobdb", p.destination)
	}
	return p.Provider.Submit(ctx, adapted)
}
