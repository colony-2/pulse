package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/colony-2/cortex/pkg/compute"
)

func (p *Provider) List(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	q, err := q.Normalize()
	if err != nil {
		return compute.ListResponse{}, err
	}
	if err = p.acquire(ctx); err != nil {
		return compute.ListResponse{}, err
	}
	defer p.release()
	if p.cfg.Kind == "ecs" {
		return p.listECS(ctx, q)
	}
	token, err := p.token(ctx)
	if err != nil {
		return compute.ListResponse{}, fmt.Errorf("cloud authentication unavailable")
	}
	if p.cfg.Kind == "cloudrun" {
		return p.listGoogle(ctx, token, q)
	}
	return p.listAzure(ctx, token, q)
}

type executionContainer struct {
	Env []struct{ Name, Value string }
}

func envMetadata(containers []executionContainer) map[string]string {
	metadata := map[string]string{}
	for _, container := range containers {
		for _, env := range container.Env {
			if strings.HasPrefix(env.Name, "CORTEX_") {
				metadata[strings.ToLower(env.Name)] = env.Value
			}
		}
	}
	return compute.Correlation(metadata)
}
func (p *Provider) listGoogle(ctx context.Context, token string, q compute.ListRequest) (compute.ListResponse, error) {
	values := url.Values{"pageSize": {fmt.Sprint(q.PageSize)}}
	if q.PageToken != "" {
		values.Set("pageToken", q.PageToken)
	}
	parent := "/v2/projects/" + url.PathEscape(p.cfg.Project) + "/locations/" + url.PathEscape(p.cfg.Region) + "/jobs/-/executions"
	response, _, err := p.request(ctx, token, "GET", parent+"?"+values.Encode(), nil)
	if err != nil {
		return compute.ListResponse{}, err
	}
	var rows []struct {
		Name, CreateTime, StartTime, CompletionTime, DeleteTime string
		RunningCount                                            int
		Annotations                                             map[string]string
		Conditions                                              []struct{ Type, State string }
		Template                                                struct{ Containers []executionContainer }
	}
	if raw, ok := response["executions"]; ok {
		if err = json.Unmarshal(raw, &rows); err != nil {
			return compute.ListResponse{}, err
		}
	}
	out := compute.ListResponse{Items: []compute.Instance{}, NextPageToken: rawString(response, "nextPageToken")}
	for _, row := range rows {
		terminal := row.CompletionTime != "" || row.DeleteTime != ""
		for _, condition := range row.Conditions {
			if condition.Type == "Completed" && (condition.State == "CONDITION_SUCCEEDED" || condition.State == "CONDITION_FAILED") {
				terminal = true
			}
		}
		if terminal {
			continue
		}
		metadata := envMetadata(row.Template.Containers)
		if annotation := row.Annotations["cortex.colony2.dev/metadata"]; annotation != "" {
			var values map[string]string
			if err = json.Unmarshal([]byte(annotation), &values); err != nil {
				return compute.ListResponse{}, fmt.Errorf("invalid execution correlation metadata")
			}
			metadata = compute.Correlation(values)
		}
		if metadata["cortex_managed_by"] != "cortex" || metadata["cortex_launch_id"] == "" {
			continue
		}
		id := metadata["cortex_launch_id"]
		if q.LaunchID != "" && id != q.LaunchID {
			continue
		}
		state := "starting"
		if row.RunningCount > 0 {
			state = "running"
		}
		out.Items = append(out.Items, compute.Instance{ID: row.Name, LaunchID: id, State: state, Metadata: metadata, Refs: []string{row.Name}, CreatedAt: compute.Timestamp(row.CreateTime), StartedAt: compute.Timestamp(row.StartTime)})
	}
	return out, nil
}
func (p *Provider) listECS(ctx context.Context, q compute.ListRequest) (compute.ListResponse, error) {
	if err := p.ecsClient(ctx); err != nil {
		return compute.ListResponse{}, err
	}
	input := &ecs.ListTasksInput{Cluster: aws.String(p.cfg.Cluster), MaxResults: aws.Int32(int32(q.PageSize)), DesiredStatus: types.DesiredStatusRunning}
	if q.PageToken != "" {
		input.NextToken = aws.String(q.PageToken)
	}
	result, err := p.ecs.ListTasks(ctx, input)
	if err != nil {
		return compute.ListResponse{}, err
	}
	out := compute.ListResponse{Items: []compute.Instance{}, NextPageToken: aws.ToString(result.NextToken)}
	if len(result.TaskArns) == 0 {
		return out, nil
	}
	details, err := p.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: aws.String(p.cfg.Cluster), Tasks: result.TaskArns, Include: []types.TaskField{types.TaskFieldTags}})
	if err != nil {
		return compute.ListResponse{}, err
	}
	for _, failure := range details.Failures {
		if aws.ToString(failure.Reason) != "MISSING" {
			return compute.ListResponse{}, fmt.Errorf("ECS task description failed")
		}
	}
	for _, task := range details.Tasks {
		state := ""
		switch aws.ToString(task.LastStatus) {
		case "PROVISIONING", "PENDING", "ACTIVATING":
			state = "starting"
		case "RUNNING":
			state = "running"
		case "DEACTIVATING", "STOPPING":
			state = "stopping"
		default:
			continue
		}
		metadata := map[string]string{}
		for _, tag := range task.Tags {
			metadata[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
		metadata = compute.Correlation(metadata)
		id := metadata["cortex_launch_id"]
		if metadata["cortex_managed_by"] != "cortex" || id == "" || (q.LaunchID != "" && id != q.LaunchID) {
			continue
		}
		ref := aws.ToString(task.TaskArn)
		out.Items = append(out.Items, compute.Instance{ID: ref, LaunchID: id, State: state, Metadata: metadata, Refs: []string{ref}, CreatedAt: task.CreatedAt, StartedAt: task.StartedAt})
	}
	return out, nil
}

// Azure exposes execution pages per job. The cursor tracks both list levels,
// so no registry or persistent snapshot is required between calls.
type azureCursor struct {
	Scope           string `json:"scope"`
	Launch          string `json:"launch"`
	JobsPage        string `json:"jobs_page"`
	JobOffset       int    `json:"job_offset"`
	ExecutionPage   string `json:"execution_page"`
	ExecutionOffset int    `json:"execution_offset"`
}

func (p *Provider) listAzure(ctx context.Context, token string, q compute.ListRequest) (compute.ListResponse, error) {
	root := "/subscriptions/" + url.PathEscape(p.cfg.Subscription) + "/resourceGroups/" + url.PathEscape(p.cfg.ResourceGroup) + "/providers/Microsoft.App/jobs"
	initial := root + "?api-version=2025-07-01"
	cursor := azureCursor{Scope: root, Launch: q.LaunchID, JobsPage: initial}
	if q.PageToken != "" {
		data, err := base64.RawURLEncoding.DecodeString(q.PageToken)
		if err != nil {
			return compute.ListResponse{}, fmt.Errorf("invalid Azure cursor")
		}
		if err = json.Unmarshal(data, &cursor); err != nil || cursor.Scope != root || cursor.Launch != q.LaunchID || cursor.JobOffset < 0 || cursor.ExecutionOffset < 0 {
			return compute.ListResponse{}, fmt.Errorf("invalid Azure cursor")
		}
	}
	out := compute.ListResponse{Items: []compute.Instance{}}
	calls := 0
	read := func(page, path string) (map[string]json.RawMessage, error) {
		calls++
		if calls > 1000 {
			return nil, fmt.Errorf("Azure listing exceeded scan limit")
		}
		u, err := url.Parse(page)
		if err != nil {
			return nil, fmt.Errorf("invalid Azure continuation")
		}
		base, _ := url.Parse(p.BaseURL)
		if (u.IsAbs() && (u.Scheme != base.Scheme || u.Host != base.Host)) || u.User != nil || u.Fragment != "" || !strings.EqualFold(u.Path, path) {
			return nil, fmt.Errorf("invalid Azure continuation")
		}
		data, _, err := p.request(ctx, token, "GET", u.RequestURI(), nil)
		return data, err
	}
	for cursor.JobsPage != "" {
		jobs, err := read(cursor.JobsPage, root)
		if err != nil {
			return compute.ListResponse{}, err
		}
		var parents []struct {
			ID   string
			Tags map[string]string
		}
		if err = json.Unmarshal(jobs["value"], &parents); err != nil {
			return compute.ListResponse{}, err
		}
		for cursor.JobOffset < len(parents) {
			job := parents[cursor.JobOffset]
			metadata := compute.Correlation(job.Tags)
			id := metadata["cortex_launch_id"]
			if metadata["cortex_managed_by"] != "cortex" || id == "" || (q.LaunchID != "" && id != q.LaunchID) {
				cursor.JobOffset++
				cursor.ExecutionPage = ""
				cursor.ExecutionOffset = 0
				continue
			}
			if len(job.ID) <= len(root)+1 || !strings.EqualFold(job.ID[:len(root)+1], root+"/") {
				return compute.ListResponse{}, fmt.Errorf("invalid Azure job ID")
			}
			suffix := job.ID[len(root)+1:]
			if strings.ContainsAny(suffix, "/?#") {
				return compute.ListResponse{}, fmt.Errorf("invalid Azure job ID")
			}
			path := job.ID + "/executions"
			if cursor.ExecutionPage == "" {
				cursor.ExecutionPage = path + "?api-version=2025-07-01"
			}
			for cursor.ExecutionPage != "" {
				executions, err := read(cursor.ExecutionPage, path)
				if err != nil {
					return compute.ListResponse{}, err
				}
				var rows []struct {
					ID         string
					Properties struct{ Status, StartTime, EndTime string }
				}
				if err = json.Unmarshal(executions["value"], &rows); err != nil {
					return compute.ListResponse{}, err
				}
				for cursor.ExecutionOffset < len(rows) {
					row := rows[cursor.ExecutionOffset]
					cursor.ExecutionOffset++
					state := ""
					switch row.Properties.Status {
					case "Running":
						state = "running"
					case "Processing":
						state = "starting"
					default:
						continue
					}
					if row.Properties.EndTime != "" {
						continue
					}
					out.Items = append(out.Items, compute.Instance{ID: row.ID, LaunchID: id, State: state, Metadata: metadata, Refs: []string{row.ID}, StartedAt: compute.Timestamp(row.Properties.StartTime)})
					if len(out.Items) == q.PageSize {
						data, _ := json.Marshal(cursor)
						out.NextPageToken = base64.RawURLEncoding.EncodeToString(data)
						return out, nil
					}
				}
				next := rawString(executions, "nextLink")
				if next != "" && next == cursor.ExecutionPage {
					return compute.ListResponse{}, fmt.Errorf("Azure pagination did not advance")
				}
				cursor.ExecutionPage = next
				cursor.ExecutionOffset = 0
			}
			cursor.JobOffset++
		}
		next := rawString(jobs, "nextLink")
		if next != "" && next == cursor.JobsPage {
			return compute.ListResponse{}, fmt.Errorf("Azure pagination did not advance")
		}
		cursor.JobsPage = next
		cursor.JobOffset = 0
	}
	return out, nil
}
