// Package remote implements the v1 OpenAPI compute-provider client.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/colony-2/cortex/pkg/compute"
)

const maxResponse = 16 << 20

type Client struct {
	base  *url.URL
	http  *http.Client
	Token func() (string, error)
}

func New(endpoint string, client *http.Client, token func() (string, error), allowHTTP bool) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return nil, fmt.Errorf("provider endpoint requires HTTPS and no embedded credentials/query")
	}
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u, http: &copyClient, Token: token}, nil
}
func (c *Client) call(ctx context.Context, method, path string, body any, out any) (int, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
	}
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != nil {
		token, e := c.Token()
		if e != nil {
			return 0, e
		}
		if token == "" {
			return 0, fmt.Errorf("provider bearer token is empty")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("provider transport failure: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(b) > maxResponse {
		return resp.StatusCode, fmt.Errorf("provider response exceeds limit")
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	if err = json.Unmarshal(b, out); err != nil {
		return resp.StatusCode, fmt.Errorf("invalid provider JSON: %w", err)
	}
	return resp.StatusCode, nil
}
func requestWire(r compute.Request) map[string]any {
	m := map[string]any{"launch_id": r.LaunchID, "image": r.Image, "platform": r.Platform, "cpu_millis": r.CPUMillis, "memory_bytes": r.MemoryBytes, "scratch_bytes": r.ScratchBytes, "timeout_seconds": r.TimeoutSeconds, "metadata": r.Metadata}
	if r.StartBefore != nil {
		m["start_before"] = r.StartBefore
	}
	return m
}
func (c *Client) Prepare(ctx context.Context, rs []compute.Request) ([]compute.Preparation, error) {
	if err := compute.ValidateBatch(rs); err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, r := range rs {
		items = append(items, requestWire(r))
	}
	var response struct {
		Results []compute.Preparation `json:"results"`
	}
	_, err := c.call(ctx, "POST", "/v1/prepare", map[string]any{"items": items}, &response)
	if err != nil {
		return nil, err
	}
	return response.Results, nil
}
func (c *Client) Submit(ctx context.Context, ls []compute.PreparedLaunch) ([]compute.Submission, error) {
	if len(ls) == 0 || len(ls) > compute.MaxBatch {
		return nil, fmt.Errorf("invalid batch size")
	}
	type item struct {
		LaunchID string          `json:"launch_id"`
		Token    string          `json:"plan_token"`
		Process  compute.Process `json:"process"`
	}
	items := []item{}
	ids := map[string]bool{}
	for _, l := range ls {
		if ids[l.LaunchID] || l.LaunchID == "" || l.Plan.Token == "" {
			return nil, fmt.Errorf("invalid or duplicate launch")
		}
		ids[l.LaunchID] = true
		if err := l.Process.Validate(); err != nil {
			return nil, err
		}
		p := l.Process
		if p.Args == nil {
			p.Args = []string{}
		}
		if p.Env == nil {
			p.Env = map[string]string{}
		}
		items = append(items, item{l.LaunchID, l.Plan.Token, p})
	}
	var response struct {
		Results []json.RawMessage `json:"results"`
	}
	code, err := c.call(ctx, "POST", "/v1/submit", map[string]any{"items": items}, &response)
	if err != nil {
		status := compute.Unknown
		switch code {
		case 400, 401, 403, 413, 422:
			status = compute.Rejected
		}
		out := []compute.Submission{}
		for _, l := range ls {
			out = append(out, compute.Submission{LaunchID: l.LaunchID, Status: status, Reason: err.Error()})
		}
		return out, err
	}
	out := []compute.Submission{}
	for _, raw := range response.Results {
		var r compute.Submission
		if e := json.Unmarshal(raw, &r); e != nil {
			continue
		}
		if !ids[r.LaunchID] {
			return nil, fmt.Errorf("unexpected launch ID in provider response")
		}
		valid := r.Status.Valid() && r.Status != compute.Prepared
		if r.Status == compute.Accepted {
			var keys map[string]json.RawMessage
			_ = json.Unmarshal(raw, &keys)
			valid = valid && r.InspectionURI != "" && len(keys["refs"]) > 0 && string(keys["refs"]) != "null"
			u, e := url.Parse(r.InspectionURI)
			valid = valid && e == nil
			if e == nil && u.IsAbs() {
				valid = valid && u.Scheme == c.base.Scheme && u.Host == c.base.Host
			}
		}
		if r.Status != compute.Accepted && r.Reason == "" {
			valid = false
		}
		if !valid {
			r.Status = compute.Unknown
			r.Reason = "invalid per-item provider response"
		}
		out = append(out, r)
	}
	return out, nil
}

// Inspect is diagnostic only and never used to bypass the scheduler cooldown.
func (c *Client) Inspect(ctx context.Context, id string) (json.RawMessage, error) {
	if strings.ContainsAny(id, "/?#") || id == "" {
		return nil, fmt.Errorf("invalid launch ID")
	}
	var out json.RawMessage
	_, err := c.call(ctx, "GET", "/v1/launches/"+url.PathEscape(id), nil, &out)
	return out, err
}
