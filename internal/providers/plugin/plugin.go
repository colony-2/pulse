// Package plugin connects Pulse's registry to the public provider SDK.
package plugin

import (
	"context"
	"github.com/colony-2/pulse/internal/config"
	"github.com/colony-2/pulse/pkg/providerplugin"
	"time"
)

func New(ctx context.Context, name string, c *config.PluginConfig, base string, callTimeout time.Duration) (*providerplugin.Process, error) {
	options, e := c.ProcessConfig(name, base)
	if e != nil {
		return nil, e
	}
	options.CallTimeout = callTimeout
	return providerplugin.Start(ctx, options)
}
