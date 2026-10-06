package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
)

// ResolveSocket selects a local Docker endpoint without requiring the Docker
// CLI. Explicit configuration wins, followed by DOCKER_CONTEXT, DOCKER_HOST,
// the saved current context, and the platform's default socket.
func ResolveSocket(configured string) (string, error) {
	home, _ := os.UserHomeDir()
	return resolveSocket(configured, runtime.GOOS, home)
}

func resolveSocket(configured, goos, home string) (string, error) {
	if configured != "" {
		return localSocket(configured)
	}
	contextName := os.Getenv("DOCKER_CONTEXT")
	if contextName == "" || contextName == "default" {
		if host := os.Getenv("DOCKER_HOST"); host != "" {
			return localSocket(host)
		}
	}
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" && home != "" {
		dir = filepath.Join(home, ".docker")
	}
	if contextName == "" && dir != "" {
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("read Docker configuration: %w", err)
		}
		if err == nil {
			var cfg struct {
				CurrentContext string `json:"currentContext"`
			}
			if err := json.Unmarshal(data, &cfg); err != nil {
				return "", fmt.Errorf("parse Docker configuration: %w", err)
			}
			contextName = cfg.CurrentContext
		}
	}
	if contextName != "" && contextName != "default" {
		if dir == "" {
			return "", fmt.Errorf("cannot locate Docker context %q: set DOCKER_CONFIG", contextName)
		}
		// Docker stores context metadata under the SHA-256 of its name.
		sum := sha256.Sum256([]byte(contextName))
		path := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read Docker context %q: %w", contextName, err)
		}
		var meta struct {
			Name      string
			Endpoints map[string]struct{ Host string }
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return "", fmt.Errorf("parse Docker context %q: %w", contextName, err)
		}
		if meta.Name != contextName {
			return "", fmt.Errorf("Docker context name mismatch for %q", contextName)
		}
		return localSocket(meta.Endpoints["docker"].Host)
	}
	if contextName == "" && goos == "darwin" && home != "" {
		path := filepath.Join(home, ".docker", "run", "docker.sock")
		if stat, err := os.Stat(path); err == nil && stat.Mode()&os.ModeSocket != 0 {
			return localSocket("unix://" + path)
		}
	}
	return localSocket("unix:///var/run/docker.sock")
}

func localSocket(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || !filepath.IsAbs(u.Path) || u.Path == "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("Docker requires a local unix:/// socket; configure providers.<name>.socket or select a local Docker context")
	}
	path := filepath.Clean(u.Path)
	// Normalize aliases (including Docker Desktop's /var/run symlink) before
	// provider construction so aliases share a single admission owner.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return "unix://" + path, nil
}
