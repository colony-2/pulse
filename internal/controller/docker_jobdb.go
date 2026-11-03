package controller

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"strings"

	provider "github.com/colony-2/pulse-provider-api"
)

// Discovery and claiming use the controller's URL. Only the job reference handed to
// a local Docker worker needs the container-to-host address. Adapt at provider
// selection so a cloud/remote fallback still receives the original job reference.
func dockerJobDBEndpoint(kind string, p provider.Provider, endpoint string, log *slog.Logger) provider.Provider {
	if kind != "docker" {
		return p
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return p
	}
	host := u.Hostname()
	if !strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") && !net.ParseIP(host).IsLoopback() {
		return p
	}
	port := u.Port()
	u.Host = "host.docker.internal"
	if port != "" {
		u.Host = net.JoinHostPort(u.Host, port)
	}
	return &dockerJobDBProvider{Provider: p, source: endpoint, destination: u.String(), log: log}
}

type dockerJobDBProvider struct {
	provider.Provider
	source, destination string
	log                 *slog.Logger
}

func (p *dockerJobDBProvider) Submit(ctx context.Context, launches []provider.Launch) ([]provider.Submission, error) {
	adapted := append([]provider.Launch(nil), launches...)
	changed := false
	for i, launch := range launches {
		if launch.Job.JobDB == p.source {
			adapted[i].Job.JobDB = p.destination
			changed = true
		}
	}
	if changed {
		p.log.Info("using host JobDB address for Docker workers", "controller_jobdb", p.source, "worker_jobdb", p.destination)
	}
	return p.Provider.Submit(ctx, adapted)
}
