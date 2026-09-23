package docker

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
)

// List reads Docker directly; it does not reconcile or alter capacity charges.
func (p *Provider) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	if _, err := q.Normalize(); err != nil {
		return compute.ListResponse{}, err
	}
	labels := []string{"pulse_managed_by=pulse"}
	if q.LaunchID != "" {
		labels = append(labels, "pulse_launch_id="+q.LaunchID)
	}
	filter, _ := json.Marshal(map[string][]string{"label": labels})
	values := url.Values{"all": {"1"}, "filters": {string(filter)}}
	var rows []struct {
		ID      string `json:"Id"`
		State   string
		Labels  map[string]string
		Created int64
	}
	if err := p.e.call(ctx, "GET", "/containers/json?"+values.Encode(), nil, &rows); err != nil {
		return compute.ListResponse{}, err
	}
	items := []compute.Instance{}
	for _, row := range rows {
		state := ""
		switch row.State {
		case "created":
			state = "starting"
		case "running":
			state = "running"
		case "paused":
			state = "paused"
		case "restarting":
			state = "starting"
		case "removing":
			state = "stopping"
		default:
			continue
		}
		metadata := compute.Correlation(row.Labels)
		if metadata["pulse_managed_by"] != "pulse" || metadata["pulse_launch_id"] == "" {
			continue
		}
		item := compute.Instance{ID: row.ID, LaunchID: metadata["pulse_launch_id"], State: state, Metadata: metadata, Refs: []string{row.ID}}
		if row.Created > 0 {
			t := time.Unix(row.Created, 0).UTC()
			item.CreatedAt = &t
		}
		items = append(items, item)
	}
	return compute.Page(items, q)
}
