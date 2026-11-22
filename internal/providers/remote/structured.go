package remote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/launchlogs"
	"github.com/colony-2/pulse/internal/providerutil"
)

// Structured connects to an HTTP endpoint that accepts provider API assignments.
type Structured struct{ client *Client }

var _ provider.Provider = (*Structured)(nil)

func NewStructured(endpoint string, client *http.Client, token func() (string, error), allowHTTP bool) (*Structured, error) {
	c, err := New(endpoint, client, token, allowHTTP)
	if err != nil {
		return nil, err
	}
	c.redactErrorBody = true
	return &Structured{client: c}, nil
}

func (p *Structured) GetCapabilities(ctx context.Context) (provider.Capabilities, error) {
	var out *provider.Capabilities
	if _, err := p.client.call(ctx, http.MethodGet, "/v1/capabilities", nil, &out); err != nil {
		return provider.Capabilities{}, err
	}
	if out == nil {
		return provider.Capabilities{}, errors.New("invalid provider capability response")
	}
	if err := providerutil.Capabilities(*out); err != nil {
		return provider.Capabilities{}, err
	}
	return *out, nil
}

func (p *Structured) Submit(ctx context.Context, launches []provider.Launch, _ provider.SubmitOptions) ([]provider.Submission, provider.LogSubscription, error) {
	if err := providerutil.Launches(launches); err != nil {
		return nil, nil, err
	}
	request := struct {
		Items []provider.Launch `json:"items"`
	}{Items: launches}
	var response struct {
		Results []json.RawMessage `json:"results"`
		Error   json.RawMessage   `json:"error"`
	}
	if _, err := p.client.call(ctx, http.MethodPost, "/v1/submit", request, &response); err != nil {
		return nil, nil, err
	}
	var methodErr error
	malformed := response.Results == nil
	if len(response.Error) > 0 {
		var message *string
		if err := json.Unmarshal(response.Error, &message); err != nil {
			malformed = true
		} else if message != nil {
			methodErr = errors.New(*message)
		}
	}
	results := make([]provider.Submission, 0, len(response.Results))
	for _, raw := range response.Results {
		var result provider.Submission
		if err := json.Unmarshal(raw, &result); err != nil {
			// Preserve a decoded ID so a malformed duplicate cannot be ignored.
			result.Status = ""
			malformed = true
		}
		results = append(results, result)
	}
	if malformed {
		methodErr = errors.Join(methodErr, errors.New("invalid provider submission response"))
	}
	return results, nil, methodErr
}

func (p *Structured) List(ctx context.Context, q provider.ListRequest) (provider.ListResponse, error) {
	q, err := providerutil.Normalize(q)
	if err != nil {
		return provider.ListResponse{}, err
	}
	var out provider.ListResponse
	if _, err := p.client.call(ctx, http.MethodGet, listPath(q.PageSize, q.PageToken, q.LaunchID), nil, &out); err != nil {
		return provider.ListResponse{}, err
	}
	if out.Items == nil || len(out.Items) > q.PageSize || len(out.NextPageToken) > 16384 || (out.NextPageToken != "" && out.NextPageToken == q.PageToken) {
		return provider.ListResponse{}, fmt.Errorf("invalid provider list page")
	}
	seen := map[string]bool{}
	for _, item := range out.Items {
		if err := providerutil.Instance(item); err != nil || seen[item.ID] || (q.LaunchID != "" && item.LaunchID != q.LaunchID) {
			return provider.ListResponse{}, fmt.Errorf("invalid provider instance")
		}
		seen[item.ID] = true
	}
	return out, nil
}

func (p *Structured) GetLaunchLog(ctx context.Context, q provider.LaunchLogRequest) (provider.LaunchLogResponse, error) {
	q, err := providerutil.NormalizeLogRequest(q)
	if err != nil {
		return provider.LaunchLogResponse{}, err
	}
	query := url.Values{"page_size": {strconv.Itoa(q.PageSize)}}
	if q.PageToken != "" {
		query.Set("page_token", base64.RawURLEncoding.EncodeToString([]byte(q.PageToken)))
	}
	// Dot-segment IDs are valid in the provider API. Escape the whole segment
	// so a proxy cannot interpret a dot or dot-dot as a different resource.
	path := "/v1/launches/" + strings.ReplaceAll(url.PathEscape(q.LaunchID), ".", "%2E") + "/logs?" + query.Encode()
	var page launchlogs.Page
	status, err := p.client.call(ctx, http.MethodGet, path, nil, &page)
	if err != nil {
		switch status {
		case http.StatusBadRequest:
			err = provider.ErrLaunchLogInvalidRequest
			if q.PageToken != "" {
				err = provider.ErrLaunchLogCursor
			}
		case http.StatusNotFound:
			err = provider.ErrLaunchLogNotFound
		case http.StatusMethodNotAllowed, http.StatusNotImplemented:
			err = provider.ErrLaunchLogUnsupported
		}
		return provider.LaunchLogResponse{}, err
	}
	if len(page.NextPageToken) > base64.RawURLEncoding.EncodedLen(providerutil.MaxLogPageToken) {
		return provider.LaunchLogResponse{}, fmt.Errorf("invalid provider launch log cursor")
	}
	token, err := base64.RawURLEncoding.Strict().DecodeString(page.NextPageToken)
	if err != nil {
		return provider.LaunchLogResponse{}, fmt.Errorf("invalid provider launch log cursor")
	}
	out := page.Response(string(token))
	if err := providerutil.ValidateLogPage(q, out); err != nil {
		return provider.LaunchLogResponse{}, err
	}
	return out, nil
}
