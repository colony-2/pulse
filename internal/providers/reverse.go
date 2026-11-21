package providers

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	api "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/config"
	pluginprovider "github.com/colony-2/pulse/internal/providers/plugin"
)

type ReverseServer struct {
	*sdk.Collector
	Address   string
	Providers map[string]api.Provider
}

// StartReverse opens the listener for the controller's lifetime. A configuration
// check loads credentials without opening a listening socket.
func StartReverse(ctx context.Context, cfg *config.Config, check bool) (*ReverseServer, error) {
	if cfg.Reverse == nil {
		return nil, nil
	}
	settings := cfg.Reverse
	options := sdk.CollectorOptions{
		AuthMode: sdk.ReverseAuthMutualKey, MutualKeys: make(map[string][]byte), CallTimeout: cfg.Call,
	}
	for name, p := range cfg.Providers {
		if p.Type != "reverse" {
			continue
		}
		key, err := sdk.LoadMutualKey(sdk.MutualKeySource{EnvName: p.TokenEnv, LookupEnv: os.LookupEnv})
		if err != nil {
			return nil, fmt.Errorf("invalid shared key for reverse provider %s in %s", name, p.TokenEnv)
		}
		options.MutualKeys[name] = key
	}
	options.MaxProviders = len(options.MutualKeys)
	if settings.Transport == "http" {
		options.Transport = sdk.ReverseTransportHTTP
	} else {
		path := func(value string) string {
			if filepath.IsAbs(value) {
				return value
			}
			return filepath.Join(cfg.BaseDir, value)
		}
		cert, err := tls.LoadX509KeyPair(path(settings.TLSCert), path(settings.TLSKey))
		if err != nil {
			return nil, fmt.Errorf("load reverse collector TLS certificate: %w", err)
		}
		options.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{sdk.ReverseKeyALPN},
		}
	}
	if check {
		return nil, nil
	}
	listener, err := net.Listen("tcp", settings.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen for reverse providers: %w", err)
	}
	collector, err := sdk.StartCollector(ctx, listener, options)
	if err != nil {
		listener.Close()
		return nil, fmt.Errorf("start reverse provider collector: %w", err)
	}
	instances := make(map[string]api.Provider, len(options.MutualKeys))
	for name := range options.MutualKeys {
		instances[name] = pluginprovider.NewReverse(collector, name)
	}
	slog.Info("reverse provider collector listening", "address", listener.Addr().String(), "transport", settings.Transport)
	return &ReverseServer{Collector: collector, Address: listener.Addr().String(), Providers: instances}, nil
}
