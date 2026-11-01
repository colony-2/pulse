// This reference adapter targets the deterministic HTTP backend used by its
// integration test. It does not claim support for a production compute backend.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
	"github.com/colony-2/pulse/pkg/providerplugin"
)

type settings struct {
	Endpoint  string `yaml:"endpoint"`
	TokenEnv  string `yaml:"token_env"`
	TokenFile string `yaml:"token_file"`
}
type provider struct {
	endpoint string
	token    string
	http     *http.Client
}

func initialize(ctx context.Context, i providerplugin.Init) (compute.Provider, error) {
	var c settings
	if providerplugin.DecodeConfig(i.ConfigYAML, &c) != nil {
		return nil, providerplugin.ErrConfig
	}
	u, e := url.Parse(c.Endpoint)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (c.TokenEnv != "" && c.TokenFile != "") {
		return nil, providerplugin.ErrConfig
	}
	token := ""
	if c.TokenEnv != "" {
		token = os.Getenv(c.TokenEnv)
		if token == "" {
			return nil, providerplugin.ErrConfig
		}
	}
	if c.TokenFile != "" {
		path, e := providerplugin.ResolvePath(i.BaseDir, c.TokenFile)
		if e != nil {
			return nil, e
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, providerplugin.ErrConfig
		}
		token = string(bytes.TrimSpace(b))
		if token == "" {
			return nil, providerplugin.ErrConfig
		}
	}
	p := &provider{endpoint: c.Endpoint, token: token, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	var ready struct {
		Ready bool `json:"ready"`
	}
	if p.call(ctx, "/health", struct{}{}, &ready) != nil || !ready.Ready {
		return nil, providerplugin.ErrInitialization
	}
	return p, nil
}

// The native API must honor deadlines/constraints and must never retry an
// ambiguous launch. HTTP POST requests have no automatic application retry.
func (p *provider) call(ctx context.Context, path string, in, out any) error {
	b, e := json.Marshal(in)
	if e != nil {
		return errors.New("native request invalid")
	}
	r, e := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+path, bytes.NewReader(b))
	if e != nil {
		return errors.New("native request invalid")
	}
	r.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		r.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, e := p.http.Do(r)
	if e != nil {
		return errors.New("native transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("native operation failed")
	}
	b, e = io.ReadAll(io.LimitReader(response.Body, providerplugin.DefaultMaxMessageBytes+1))
	if e != nil || len(b) > providerplugin.DefaultMaxMessageBytes {
		return errors.New("native response invalid")
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("native response invalid")
	}
	return nil
}
func (p *provider) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	var out []compute.Submission
	e := p.call(ctx, "/submit", ls, &out)
	return out, e
}
func (p *provider) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	var out compute.ListResponse
	e := p.call(ctx, "/list", q, &out)
	return out, e
}
func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println("pulse-provider-fixture 1.0.0")
			return
		case "--help", "-h", "help":
			fmt.Println("Pulse reference instance provider. Configure through providers.<name>.plugin.config; commands: version, --help")
			return
		default:
			fmt.Fprintln(os.Stderr, "unknown option")
			os.Exit(2)
		}
	}
	providerplugin.Serve(initialize, providerplugin.ServeOptions{Implementation: "pulse-provider-fixture", Version: "1.0.0"})
}
