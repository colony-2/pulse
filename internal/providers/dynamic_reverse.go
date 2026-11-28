package providers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/config"
)

const dynamicCheckpointInterval = time.Minute

var dynamicProviderName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type DynamicReverseServer struct {
	Address   string
	collector *sdk.Collector
	registry  *dynamicRegistry
	done      chan struct{}
	err       error
}

func StartDynamicReverse(ctx context.Context, cfg *config.Config, check bool) (*DynamicReverseServer, error) {
	return startDynamicReverse(ctx, cfg, check, time.Now)
}

func startDynamicReverse(ctx context.Context, cfg *config.Config, check bool, now func() time.Time) (*DynamicReverseServer, error) {
	settings := cfg.DynamicProvider
	if settings == nil {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(reversePath(cfg.BaseDir, settings.TLSCert), reversePath(cfg.BaseDir, settings.TLSKey))
	if err != nil {
		return nil, fmt.Errorf("load dynamic provider listener TLS certificate: %w", err)
	}
	caPEM, err := os.ReadFile(reversePath(cfg.BaseDir, settings.Auth.ClientCA))
	if err != nil {
		return nil, fmt.Errorf("read dynamic provider client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("dynamic provider client CA contains no valid certificates")
	}
	registry, err := openDynamicRegistry(reversePath(cfg.BaseDir, settings.StateFile), cfg.Providers, now)
	if err != nil || check {
		return nil, err
	}
	observing := make(chan struct{})
	options := sdk.CollectorOptions{
		AuthMode: sdk.ReverseAuthMTLS, Transport: sdk.ReverseTransportTLS, CallTimeout: cfg.Call,
		CallbackQueue: 1024,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{sdk.ReverseALPN},
			ClientCAs: clientCAs, ClientAuth: tls.RequireAndVerifyClientCert,
		},
		Authorize: func(ctx context.Context, state tls.ConnectionState) (string, error) {
			select {
			case <-observing:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
				return "", sdk.ErrInvalid
			}
			name := state.PeerCertificates[0].Subject.CommonName
			if _, reserved := cfg.Providers[name]; reserved || !dynamicProviderName.MatchString(name) {
				return "", sdk.ErrInvalid
			}
			return name, nil
		},
	}
	listener, err := net.Listen("tcp", settings.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen for dynamic reverse providers: %w", err)
	}
	if err := registry.commit(registry.lastSeen); err != nil {
		listener.Close()
		return nil, err
	}
	collector, err := sdk.StartCollector(ctx, listener, options)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("start dynamic reverse provider collector: %w", err)
	}
	registry.collector = collector
	failures := make(chan error, 1)
	observer, err := collector.OnProvidersChanged(context.Background(), func(_ context.Context, change sdk.ProviderChange) {
		if err := registry.record(change); err != nil {
			select {
			case failures <- err:
			default:
			}
		}
	})
	if err != nil {
		collector.Close()
		return nil, fmt.Errorf("observe dynamic provider registrations: %w", err)
	}
	close(observing)
	server := &DynamicReverseServer{Address: listener.Addr().String(), collector: collector, registry: registry, done: make(chan struct{})}
	go server.run(observer, failures)
	slog.Info("dynamic reverse provider collector listening", "address", server.Address)
	return server, nil
}

func (server *DynamicReverseServer) run(observer *sdk.ProviderSubscription, failures <-chan error) {
	defer close(server.done)
	observerDone := make(chan error, 1)
	go func() { observerDone <- observer.Wait() }()
	ticker := time.NewTicker(dynamicCheckpointInterval)
	defer ticker.Stop()
	for server.err == nil {
		select {
		case err := <-observerDone:
			server.err = errors.Join(err, server.collector.Close())
			select {
			case failure := <-failures:
				server.err = errors.Join(failure, server.err)
			default:
			}
			return
		case server.err = <-failures:
		case <-ticker.C:
			server.err = server.registry.checkpoint()
		}
	}
	server.err = errors.Join(server.err, server.collector.Close(), <-observerDone)
}

func (server *DynamicReverseServer) Wait() error {
	<-server.done
	return server.err
}

func (server *DynamicReverseServer) Close() error {
	_ = server.collector.Close()
	return server.Wait()
}

func (server *DynamicReverseServer) Providers() map[string]api.Provider {
	return server.registry.providers()
}
