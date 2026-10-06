package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	c2jconfig "github.com/colony-2/c2j/pkg/config"
)

// LoadSource loads optional Pulse settings and applies c2j tenant conventions.
func LoadSource(path string) (*Config, error) {
	return LoadOptions(context.Background(), path, "")
}

// LoadOptions gives an explicit JobDB URI precedence over configured targets.
// Pulse sources are selected, never merged: file, PULSE_CONFIG, then pulse.yaml.
func LoadOptions(ctx context.Context, path, jobdb string) (*Config, error) {
	var data []byte
	var err error
	source := path
	if path != "" {
		data, err = os.ReadFile(path)
	} else if value, ok := os.LookupEnv("PULSE_CONFIG"); ok {
		source = "PULSE_CONFIG"
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("PULSE_CONFIG must contain a YAML configuration")
		}
		data = []byte(value)
	} else {
		source = "pulse.yaml"
		data, err = os.ReadFile(source)
		if errors.Is(err, os.ErrNotExist) {
			data, err, source = []byte("{}"), nil, "defaults"
		}
	}
	if err != nil {
		return nil, err
	}
	cfg, err := parse(ctx, data, strings.TrimSpace(jobdb))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return cfg, nil
}

func resolveJobDB(ctx context.Context) (string, error) {
	if uri := strings.TrimSpace(os.Getenv("C2J_JOBDB")); uri != "" {
		return uri, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	project, err := c2jconfig.LoadProjectConfig(dir)
	if err != nil && !errors.Is(err, c2jconfig.ErrConfigNotFound) {
		return "", err
	}
	if err == nil && project != nil {
		uri, err := project.JobDBURI(ctx)
		if err != nil {
			return "", err
		}
		if uri = strings.TrimSpace(uri); uri != "" {
			return uri, nil
		}
	}
	return "", fmt.Errorf("JobDB tenant required: use --jobdb, C2J_JOBDB, or jobdb in .c2j/config.yaml")
}
