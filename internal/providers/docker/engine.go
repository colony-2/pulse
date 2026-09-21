package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type engine struct {
	client        *http.Client
	base, version string
	registryAuth  string
}
type apiError struct{ code int }

func (e *apiError) Error() string { return fmt.Sprintf("Docker API returned HTTP %d", e.code) }
func newEngine(socket string) *engine {
	return &engine{base: "http://docker", client: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(socket, "unix://"))
	}, ResponseHeaderTimeout: 30 * time.Second}}}
}
func (e *engine) call(ctx context.Context, method, path string, body any, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+e.version+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(path, "/images/create") && e.registryAuth != "" {
		req.Header.Set("X-Registry-Auth", e.registryAuth)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &apiError{resp.StatusCode}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 32<<20))
	return dec.Decode(out)
}
func (e *engine) pull(ctx context.Context, image, platform string) error {
	q := url.Values{"fromImage": {image}, "platform": {platform}}
	req, err := http.NewRequestWithContext(ctx, "POST", e.base+e.version+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if e.registryAuth != "" {
		req.Header.Set("X-Registry-Auth", e.registryAuth)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &apiError{resp.StatusCode}
	}
	d := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	for {
		var item struct {
			Error string `json:"error"`
		}
		err = d.Decode(&item)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if item.Error != "" {
			return fmt.Errorf("Docker image pull failed")
		}
	}
}
