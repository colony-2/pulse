package compute

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Instance describes provider-owned active compute, not a JobDB job status.
// A launch can have more than one native instance; ID is unique within a provider.
type Instance struct {
	ID        string            `json:"id"`
	LaunchID  string            `json:"launch_id"`
	State     string            `json:"state"`
	Metadata  map[string]string `json:"metadata"`
	Refs      []string          `json:"refs"`
	CreatedAt *time.Time        `json:"created_at,omitempty"`
	StartedAt *time.Time        `json:"started_at,omitempty"`
}

func Active(state string) bool {
	switch state {
	case "queued", "starting", "running", "paused", "stopping":
		return true
	}
	return false
}
func (i Instance) Validate() error {
	if i.ID == "" || !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(i.LaunchID) || !Active(i.State) || len(i.Metadata) == 0 || i.Refs == nil {
		return fmt.Errorf("invalid active instance")
	}
	for _, ref := range i.Refs {
		if ref == "" {
			return fmt.Errorf("invalid instance reference")
		}
	}
	return nil
}

type ListRequest struct {
	PageSize  int    `json:"page_size"`
	PageToken string `json:"page_token,omitempty"`
	LaunchID  string `json:"launch_id,omitempty"`
}

func (q ListRequest) Normalize() (ListRequest, error) {
	if q.PageSize == 0 {
		q.PageSize = 100
	}
	if q.PageSize < 1 || q.PageSize > 100 || len(q.PageToken) > 16384 || (q.LaunchID != "" && !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(q.LaunchID)) {
		return q, fmt.Errorf("invalid list parameters")
	}
	return q, nil
}

type ListResponse struct {
	Items         []Instance `json:"items"`
	NextPageToken string     `json:"next_page_token,omitempty"`
}

func Correlation(metadata map[string]string) map[string]string {
	out := map[string]string{}
	for _, key := range []string{"cortex_metadata_version", "cortex_managed_by", "cortex_jobdb_instance_id", "cortex_tenant_id", "cortex_job_id", "cortex_launch_id"} {
		if value, ok := metadata[key]; ok {
			out[key] = value
		}
	}
	return out
}
func Timestamp(value string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.IsZero() {
		return nil
	}
	return &t
}

// Page is used for APIs such as Docker that return the whole current inventory.
// The cursor is a last-seen ID, not an offset into a changing list.
func Page(items []Instance, q ListRequest) (ListResponse, error) {
	q, err := q.Normalize()
	if err != nil {
		return ListResponse{}, err
	}
	after := ""
	if q.PageToken != "" {
		data, e := base64.RawURLEncoding.DecodeString(q.PageToken)
		if e != nil {
			return ListResponse{}, fmt.Errorf("invalid page token")
		}
		var cursor struct{ After, Launch string }
		if e = json.Unmarshal(data, &cursor); e != nil || cursor.Launch != q.LaunchID {
			return ListResponse{}, fmt.Errorf("invalid page token")
		}
		after = cursor.After
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	out := ListResponse{Items: []Instance{}}
	for _, item := range items {
		if !Active(item.State) || item.ID <= after || (q.LaunchID != "" && item.LaunchID != q.LaunchID) {
			continue
		}
		if len(out.Items) == q.PageSize {
			data, _ := json.Marshal(struct{ After, Launch string }{out.Items[len(out.Items)-1].ID, q.LaunchID})
			out.NextPageToken = base64.RawURLEncoding.EncodeToString(data)
			break
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}
