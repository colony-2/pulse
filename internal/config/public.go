package config

import (
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// Public returns configuration with environment values and URL credentials
// removed. Do not expose the process environment or native credential objects.
func (c *Config) Public() (map[string]any, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err = yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	var clean func(any) any
	clean = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			for key, item := range v {
				if key == "env" {
					if values, ok := item.(map[string]any); ok {
						for name := range values {
							values[name] = "[REDACTED]"
						}
						continue
					}
				}
				v[key] = clean(item)
			}
		case []any:
			for i, item := range v {
				v[i] = clean(item)
			}
		case string:
			if u, err := url.Parse(v); err == nil && u.Scheme != "" && strings.Contains(v, "://") {
				u.User = nil
				if u.RawQuery != "" {
					u.RawQuery = "redacted"
				}
				u.Fragment = ""
				return u.String()
			}
		}
		return value
	}
	clean(out)
	return out, nil
}
