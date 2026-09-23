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
	relative, err := url.Parse(path)
	if err != nil {
		return 0, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + relative.Path
	u.RawQuery = relative.RawQuery
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
func (c *Client) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {

	if err := compute.ValidateLaunches(ls); err != nil {
		return nil, err
	}
	items := []map[string]any{}
	ids := map[string]bool{}
	for _, l := range ls {
		ids[l.LaunchID] = true
		item := requestWire(l.Request)
		p := l.Process
		if p.Args == nil {
			p.Args = []string{}
		}
		if p.Env == nil {
			p.Env = map[string]string{}
		}
		item["process"] = p
		items = append(items, item)
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
		valid := r.Status.Valid()
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

// List returns active provider instances; it never changes placement decisions.
func (c *Client) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	q, err := q.Normalize()
	if err != nil {
		return compute.ListResponse{}, err
	}
	values := url.Values{"page_size": {fmt.Sprint(q.PageSize)}}
	if q.PageToken != "" {
		values.Set("page_token", q.PageToken)
	}
	if q.LaunchID != "" {
		values.Set("launch_id", q.LaunchID)
	}
	var out compute.ListResponse
	_, err = c.call(ctx, "GET", "/v1/launches?"+values.Encode(), nil, &out)
	if err != nil {
		return compute.ListResponse{}, err
	}
	if out.Items == nil || len(out.Items) > q.PageSize || len(out.NextPageToken) > 16384 || (out.NextPageToken != "" && out.NextPageToken == q.PageToken) {
		return compute.ListResponse{}, fmt.Errorf("invalid provider list response")
	}
	seen := map[string]bool{}
	for _, item := range out.Items {
		if err := item.Validate(); err != nil || seen[item.ID] || (q.LaunchID != "" && item.LaunchID != q.LaunchID) {
			return compute.ListResponse{}, fmt.Errorf("invalid provider instance")
		}
		seen[item.ID] = true
	}
	return out, nil
}
