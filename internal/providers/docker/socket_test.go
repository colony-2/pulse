package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSocketSelection(t *testing.T) {
	for _, tc := range []struct {
		name, configured, contextEnv, hostEnv, savedContext, want string
		wantError                                                 bool
	}{
		{name: "Linux default", want: "unix:///var/run/docker.sock"},
		{name: "Docker Desktop current context", savedContext: "desktop-linux", want: "unix:///tmp/desktop.sock"},
		{name: "Linux rootless current context", savedContext: "rootless", want: "unix:///run/user/1000/docker.sock"},
		{name: "environment host overrides saved context", savedContext: "desktop-linux", hostEnv: "unix:///tmp/host.sock", want: "unix:///tmp/host.sock"},
		{name: "environment context overrides host", contextEnv: "desktop-linux", hostEnv: "unix:///tmp/host.sock", want: "unix:///tmp/desktop.sock"},
		{name: "explicit socket wins", configured: "unix:///tmp/explicit.sock", contextEnv: "missing", hostEnv: "tcp://remote:2375", want: "unix:///tmp/explicit.sock"},
		{name: "default context overrides saved context", contextEnv: "default", savedContext: "desktop-linux", want: "unix:///var/run/docker.sock"},
		{name: "default context uses environment host", contextEnv: "default", hostEnv: "unix:///tmp/host.sock", want: "unix:///tmp/host.sock"},
		{name: "missing context does not fall back", contextEnv: "missing", wantError: true},
		{name: "remote context rejected", savedContext: "remote", wantError: true},
		{name: "remote host rejected", hostEnv: "tcp://remote:2375", wantError: true},
		{name: "invalid explicit socket does not fall back", configured: "unix://relative.sock", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DOCKER_CONFIG", dir)
			t.Setenv("DOCKER_CONTEXT", tc.contextEnv)
			t.Setenv("DOCKER_HOST", tc.hostEnv)
			writeDockerJSON(t, filepath.Join(dir, "config.json"), map[string]string{"currentContext": tc.savedContext})
			for name, host := range map[string]string{
				"desktop-linux": "unix:///tmp/desktop.sock",
				"rootless":      "unix:///run/user/1000/docker.sock",
				"remote":        "ssh://example.com",
			} {
				sum := sha256.Sum256([]byte(name))
				writeDockerJSON(t, filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"), map[string]any{
					"Name": name, "Endpoints": map[string]any{"docker": map[string]string{"Host": host}},
				})
			}
			got, err := resolveSocket(tc.configured, "linux", t.TempDir())
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			// /var can itself be a symlink when these tests run on macOS.
			want, wantErr := localSocket(tc.want)
			if err != nil || wantErr != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestResolveDockerDesktopSocketWithoutSystemSymlink(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONFIG", "")
	// A short path also fits macOS's Unix socket path-length limit.
	home, err := os.MkdirTemp("", "pulse-socket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	socket := filepath.Join(home, ".docker", "run", "docker.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	got, err := resolveSocket("", "darwin", home)
	want, _ := localSocket("unix://" + socket)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	alias := filepath.Join(home, "alias.sock")
	if err := os.Symlink(socket, alias); err != nil {
		t.Fatal(err)
	}
	got, err = ResolveSocket("unix://" + alias)
	if err != nil || got != want {
		t.Fatalf("alias did not resolve: %q, %v", got, err)
	}
}

func TestInvalidDockerConfigurationDoesNotFallBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSocket(""); err == nil {
		t.Fatal("malformed Docker config was ignored")
	}
}

func writeDockerJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
