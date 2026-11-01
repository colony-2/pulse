package providerplugin

import (
	"context"
	plugin "github.com/hashicorp/go-plugin"
	"os"
	"testing"
)

func TestWrongMajorHelper(t *testing.T) {
	if os.Getenv("PULSE_PLUGIN_COOKIE") == "" {
		return
	}
	plugin.Serve(&plugin.ServeConfig{HandshakeConfig: handshake, VersionedPlugins: map[int]plugin.PluginSet{2: {InterfaceName: &rpcPlugin{}}}, GRPCServer: plugin.DefaultGRPCServer})
	os.Exit(0)
}
func TestWrongCookieHelper(t *testing.T) {
	if os.Getenv("PULSE_PLUGIN_COOKIE") == "" {
		return
	}
	h := handshake
	h.MagicCookieKey = "JOBDB_SECRET_PROVIDER_PLUGIN"
	h.MagicCookieValue = "jobdb-secret-provider"
	plugin.Serve(&plugin.ServeConfig{HandshakeConfig: h, VersionedPlugins: pluginSet(nil), GRPCServer: plugin.DefaultGRPCServer})
	os.Exit(0)
}
func TestIncompatibleProtocol(t *testing.T) {
	for _, helper := range []string{"TestWrongMajorHelper", "TestWrongCookieHelper"} {
		c := fixtureOptions(t, "accept")
		c.Args = []string{"-test.run=^" + helper + "$"}
		if _, e := Start(context.Background(), c); e == nil {
			t.Fatal("incompatible executable accepted")
		}
		if countEvents(t, c, "configure") != 0 {
			t.Fatal("incompatible executable configured")
		}
	}
}
