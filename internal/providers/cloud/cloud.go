// Package cloud implements built-in cloud adapters with explicit native request
// bodies. Authentication and ECS calls use native Go SDKs; no cloud CLI is needed.
package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/colony-2/cortex/pkg/compute"
)

const Gi int64 = 1 << 30
const Mi int64 = 1 << 20

type Config struct {
	Kind, Project, Region, ServiceAccount, Cluster, ExecutionRole, TaskRole, Subscription, ResourceGroup, EnvironmentID, SupervisorPath, TokenEnv string
	Subnets, SecurityGroups                                                                                                                       []string
	PublicIP                                                                                                                                      bool
	ImageStorageBounds                                                                                                                            map[string]int64
	MaxAzureCPU                                                                                                                                   int64
}
type Provider struct {
	cfg          Config
	HTTP         *http.Client
	accessToken  func(context.Context) (string, error)
	quotaProject string
	ecs          *ecs.Client
	BaseURL      string
	gate         chan struct{}
}
type plan struct {
	Request                        compute.Request
	Allocation                     compute.Allocation
	TotalMemory, DiskGiB, CPUUnits int64
}

func New(c Config) (*Provider, error) {
	if c.Region == "" {
		return nil, fmt.Errorf("cloud region required")
	}
	switch c.Kind {
	case "cloudrun":
		if c.Project == "" {
			return nil, fmt.Errorf("cloudrun project required")
		}
	case "ecs":
		if c.Cluster == "" || len(c.Subnets) == 0 || c.ExecutionRole == "" || c.SupervisorPath == "" {
			return nil, fmt.Errorf("ECS requires cluster, subnets, execution_role, supervisor_path")
		}
	case "azurejobs":
		if c.Subscription == "" || c.ResourceGroup == "" || c.EnvironmentID == "" {
			return nil, fmt.Errorf("Azure requires subscription, resource_group, environment_id")
		}
		if c.MaxAzureCPU == 0 {
			c.MaxAzureCPU = 2000
		}
		if c.MaxAzureCPU != 2000 && c.MaxAzureCPU != 4000 {
			return nil, fmt.Errorf("Azure max CPU must be 2000 or 4000 millicores")
		}
	default:
		return nil, fmt.Errorf("unknown cloud kind")
	}
	p := &Provider{cfg: c, gate: make(chan struct{}, 1), HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if c.Kind == "cloudrun" {
		p.BaseURL = "https://run.googleapis.com"
	} else if c.Kind == "azurejobs" {
		p.BaseURL = "https://management.azure.com"
	}
	return p, nil
}
func ceil(n, unit int64) int64 { return (n + unit - 1) / unit * unit }
func (p *Provider) size(r compute.Request) (plan, string) {
	a := r.Allocation
	a.ImageID = ""
	a.ImageDigest = ""
	if _, digest, ok := strings.Cut(r.Image, "@"); ok {
		a.ImageDigest = digest
	}
	pl := plan{Request: r, Allocation: a}
	reason := ""
	if r.StartBefore != nil {
		reason = "cloud adapter does not enforce queued start deadlines"
	}
	switch p.cfg.Kind {
	case "cloudrun":
		if r.Platform != "linux/amd64" {
			reason = "Cloud Run adapter requires linux/amd64"
		}
		if r.TimeoutSeconds > 604800 {
			reason = "Cloud Run task timeout exceeds seven days"
		}
		pl.TotalMemory = ceil(r.MemoryBytes+r.ScratchBytes, Mi)
		found := false
		for _, size := range []struct{ cpu, min, max int64 }{{1000, 512 * Mi, 4 * Gi}, {2000, 512 * Mi, 8 * Gi}, {4000, 2 * Gi, 16 * Gi}, {6000, 4 * Gi, 24 * Gi}, {8000, 4 * Gi, 32 * Gi}} {
			if size.cpu >= r.CPUMillis && max(pl.TotalMemory, size.min) <= size.max {
				pl.Allocation.CPUMillis = size.cpu
				pl.TotalMemory = max(pl.TotalMemory, size.min)
				pl.Allocation.MemoryBytes = pl.TotalMemory - r.ScratchBytes
				found = true
				break
			}
		}
		if !found {
			reason = "Cloud Run resource limits exceeded"
		}
	case "ecs":
		if r.Platform != "linux/amd64" && r.Platform != "linux/arm64" {
			reason = "unsupported Fargate platform"
		}
		bound, known := p.cfg.ImageStorageBounds[r.Image]
		if !known || bound <= 0 || !strings.Contains(r.Image, "@") {
			reason = "Fargate scratch requires a configured storage bound for this digest-pinned image"
		}
		pl.DiskGiB = max(int64(20), ceil(r.ScratchBytes+bound, Gi)/Gi)
		if pl.DiskGiB > 200 {
			reason = "Fargate ephemeral storage limit exceeded"
		}
		pl.Allocation.ScratchBytes = pl.DiskGiB*Gi - bound
		found := false
		for _, size := range []struct{ milli, units, min, max, step int64 }{{250, 256, 512, 2048, 512}, {500, 512, 1024, 4096, 1024}, {1000, 1024, 2048, 8192, 1024}, {2000, 2048, 4096, 16384, 1024}, {4000, 4096, 8192, 30720, 1024}, {8000, 8192, 16384, 61440, 4096}, {16000, 16384, 32768, 122880, 8192}} {
			if size.milli < r.CPUMillis {
				continue
			}
			memory := max(size.min, ceil(r.MemoryBytes, Mi)/Mi)
			if size.units == 256 && memory > 1024 {
				memory = 2048
			} else {
				memory = ceil(memory, size.step)
			}
			if memory <= size.max {
				pl.CPUUnits = size.units
				pl.Allocation.CPUMillis = size.milli
				pl.Allocation.MemoryBytes = memory * Mi
				found = true
				break
			}
		}
		if !found {
			reason = "Fargate CPU/memory combinations exceeded"
		}
	case "azurejobs":
		if r.Platform != "linux/amd64" {
			reason = "Azure adapter requires linux/amd64"
		}
		bound, known := p.cfg.ImageStorageBounds[r.Image]
		if !known || bound <= 0 || !strings.Contains(r.Image, "@") {
			reason = "Azure scratch requires a configured storage bound for this digest-pinned image"
		}
		found := false
		for cpu := int64(250); cpu <= p.cfg.MaxAzureCPU; cpu += 250 {
			memory := cpu * 2 * Gi / 1000
			disk := int64(8) * Gi
			switch {
			case cpu <= 250:
				disk = Gi
			case cpu <= 500:
				disk = 2 * Gi
			case cpu <= 1000:
				disk = 4 * Gi
			}
			if cpu >= r.CPUMillis && memory >= r.MemoryBytes && disk-bound >= r.ScratchBytes {
				pl.Allocation.CPUMillis = cpu
				pl.Allocation.MemoryBytes = memory
				pl.Allocation.ScratchBytes = disk - bound
				found = true
				break
			}
		}
		if !found {
			reason = "Azure consumption CPU/memory/scratch limits exceeded"
		}
	}
	return pl, reason
}

func envList(process compute.Process, metadata map[string]string) []map[string]string {
	env := map[string]string{}
	for k, v := range process.Env {
		env[k] = v
	}
	for k, v := range metadata {
		if _, supplied := env[strings.ToUpper(k)]; !supplied {
			env[strings.ToUpper(k)] = v
		}
	}
	if _, ok := env["TMPDIR"]; !ok {
		env["TMPDIR"] = "/scratch"
	}
	if _, supplied := env["CORTEX_SCRATCH_DIR"]; !supplied {
		env["CORTEX_SCRATCH_DIR"] = "/scratch"
	}
	keys := []string{}
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []map[string]string{}
	for _, k := range keys {
		out = append(out, map[string]string{"name": k, "value": env[k]})
	}
	return out
}
func nameFor(id string) string {
	h := sha256.Sum256([]byte(id))
	return "cortex-" + hex.EncodeToString(h[:12])
}
func (p *Provider) request(ctx context.Context, token, method, path string, body any) (map[string]json.RawMessage, int, error) {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, p.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.Kind == "cloudrun" && p.quotaProject != "" {
		req.Header.Set("X-Goog-User-Project", p.quotaProject)
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(data) > 16<<20 {
		return nil, resp.StatusCode, fmt.Errorf("cloud response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("cloud API HTTP %d", resp.StatusCode)
	}
	out := map[string]json.RawMessage{}
	if len(data) > 0 {
		err = json.Unmarshal(data, &out)
	}
	return out, resp.StatusCode, err
}
func rawString(m map[string]json.RawMessage, key string) string {
	var s string
	json.Unmarshal(m[key], &s)
	return s
}
func (p *Provider) waitGoogle(ctx context.Context, token string, op map[string]json.RawMessage) error {
	for {
		var done bool
		json.Unmarshal(op["done"], &done)
		if done {
			if len(op["error"]) > 0 {
				return fmt.Errorf("Google create operation failed")
			}
			return nil
		}
		name := rawString(op, "name")
		if !strings.HasPrefix(name, "projects/") {
			return fmt.Errorf("invalid Google operation")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		var err error
		op, _, err = p.request(ctx, token, "GET", "/v2/"+name, nil)
		if err != nil {
			return err
		}
	}
}
func (p *Provider) Submit(ctx context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	if err := compute.ValidateLaunches(ls); err != nil {
		return nil, err
	}
	// Native calls may be singular; one adapter batch keeps the controller uniform.
	if err := p.acquire(ctx); err != nil {
		return nil, err
	}
	defer p.release()
	out := []compute.Submission{}
	for _, l := range ls {
		result := compute.Submission{LaunchID: l.LaunchID}

		pl, reason := p.size(l.Request)
		if reason != "" {
			result.Status, result.Reason = compute.Unsupported, reason
			out = append(out, result)
			continue
		}

		if l.Process.WorkingDir != "" && p.cfg.Kind != "ecs" {
			result.Status = compute.Unsupported
			result.Reason = "working directory override unsupported"
			out = append(out, result)
			continue
		}
		var err error
		if p.cfg.Kind == "ecs" {
			result, err = p.submitECS(ctx, l, pl)
		} else {
			token, e := p.token(ctx)
			if e != nil {
				result.Status = compute.Unavailable
				result.Reason = "cloud authentication unavailable"
				out = append(out, result)
				continue
			}
			if p.cfg.Kind == "cloudrun" {
				result, err = p.submitGoogle(ctx, token, l, pl)
			} else {
				result, err = p.submitAzure(ctx, token, l, pl)
			}
		}
		if err != nil {
			result.Reason = err.Error()
		}
		out = append(out, result)
	}
	return out, nil
}
func (p *Provider) submitGoogle(ctx context.Context, token string, l compute.Launch, pl plan) (compute.Submission, error) {
	r := compute.Submission{LaunchID: l.LaunchID, Status: compute.Unavailable}
	name := nameFor(l.LaunchID)
	parent := "projects/" + url.PathEscape(p.cfg.Project) + "/locations/" + url.PathEscape(p.cfg.Region)
	resource := parent + "/jobs/" + name
	a := pl.Allocation
	container := map[string]any{"image": a.Image, "command": l.Process.Command, "args": l.Process.Args, "env": envList(l.Process, pl.Request.Metadata), "resources": map[string]any{"limits": map[string]string{"cpu": strconv.FormatInt(a.CPUMillis/1000, 10), "memory": fmt.Sprintf("%dMi", pl.TotalMemory/Mi)}}, "volumeMounts": []map[string]string{{"name": "scratch", "mountPath": "/scratch"}}}
	task := map[string]any{"containers": []any{container}, "maxRetries": 0, "timeout": fmt.Sprintf("%ds", pl.Request.TimeoutSeconds), "volumes": []any{map[string]any{"name": "scratch", "emptyDir": map[string]string{"medium": "MEMORY", "sizeLimit": fmt.Sprintf("%d", a.ScratchBytes)}}}}
	if p.cfg.ServiceAccount != "" {
		task["serviceAccount"] = p.cfg.ServiceAccount
	}
	// Exact metadata stays in an annotation and process env; labels are searchable.
	metadata, _ := json.Marshal(pl.Request.Metadata)
	body := map[string]any{"labels": map[string]string{"cortex_managed_by": "cortex", "cortex_launch_id": name}, "annotations": map[string]string{"cortex.colony2.dev/metadata": string(metadata)}, "template": map[string]any{"taskCount": 1, "parallelism": 1, "template": task}}
	op, code, err := p.request(ctx, token, "POST", "/v2/"+parent+"/jobs?jobId="+name, body)
	if err != nil {
		if code == 409 {
			r.Status = compute.Unknown
		} else if code == 429 {
			r.Status = compute.NoCapacity
		} else if code == 400 || code == 403 {
			r.Status = compute.Rejected
		}
		return r, err
	}
	r.Refs = []string{resource}
	if err = p.waitGoogle(ctx, token, op); err != nil {
		return r, err
	}
	r.Status = compute.Unknown
	op, code, err = p.request(ctx, token, "POST", "/v2/"+resource+":run", map[string]any{})
	if err != nil {
		if code == 429 {
			r.Status = compute.NoCapacity
		} else if code == 400 || code == 403 {
			r.Status = compute.Rejected
		}
		return r, err
	}
	if len(op["error"]) > 0 {
		return r, fmt.Errorf("Google run operation returned an error")
	}
	operation := rawString(op, "name")
	if operation == "" {
		return r, fmt.Errorf("Google run returned no operation reference")
	}
	r.Refs = append(r.Refs, operation)
	r.Status = compute.Accepted
	return r, nil
}
func (p *Provider) submitAzure(ctx context.Context, token string, l compute.Launch, pl plan) (compute.Submission, error) {
	r := compute.Submission{LaunchID: l.LaunchID, Status: compute.Unavailable}
	resource := "/subscriptions/" + url.PathEscape(p.cfg.Subscription) + "/resourceGroups/" + url.PathEscape(p.cfg.ResourceGroup) + "/providers/Microsoft.App/jobs/" + nameFor(l.LaunchID)
	path := resource + "?api-version=2025-07-01"
	_, code, err := p.request(ctx, token, "GET", path, nil)
	if err == nil {
		r.Status = compute.Unknown
		r.Refs = []string{resource}
		return r, fmt.Errorf("launch parent already exists; refusing duplicate start")
	}
	if code != 404 {
		return r, err
	}
	a := pl.Allocation
	container := map[string]any{"name": "executor", "image": a.Image, "command": l.Process.Command, "args": l.Process.Args, "env": envList(l.Process, pl.Request.Metadata), "resources": map[string]any{"cpu": float64(a.CPUMillis) / 1000, "memory": fmt.Sprintf("%gGi", float64(a.MemoryBytes)/float64(Gi))}, "volumeMounts": []map[string]string{{"volumeName": "scratch", "mountPath": "/scratch"}}}
	body := map[string]any{"location": p.cfg.Region, "tags": pl.Request.Metadata, "properties": map[string]any{"environmentId": p.cfg.EnvironmentID, "configuration": map[string]any{"triggerType": "Manual", "replicaTimeout": pl.Request.TimeoutSeconds, "replicaRetryLimit": 0, "manualTriggerConfig": map[string]int{"parallelism": 1, "replicaCompletionCount": 1}}, "template": map[string]any{"containers": []any{container}, "volumes": []any{map[string]string{"name": "scratch", "storageType": "EmptyDir"}}}}}
	_, code, err = p.request(ctx, token, "PUT", path, body)
	if err != nil {
		if code == 429 {
			r.Status = compute.NoCapacity
		} else if code == 400 || code == 403 {
			r.Status = compute.Rejected
		}
		return r, err
	}
	r.Refs = []string{resource}
	for {
		state, _, e := p.request(ctx, token, "GET", path, nil)
		if e != nil {
			return r, e
		}
		var props struct {
			State string `json:"provisioningState"`
		}
		json.Unmarshal(state["properties"], &props)
		if props.State == "Succeeded" {
			break
		}
		if props.State == "Failed" || props.State == "Canceled" {
			return r, fmt.Errorf("Azure parent provisioning failed")
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	r.Status = compute.Unknown
	response, code, err := p.request(ctx, token, "POST", resource+"/start?api-version=2025-07-01", nil)
	if err != nil {
		if code == 429 {
			r.Status = compute.NoCapacity
		} else if code == 400 || code == 403 {
			r.Status = compute.Rejected
		}
		return r, err
	}
	id := rawString(response, "id")
	if id == "" {
		id = rawString(response, "name")
	}
	if id == "" {
		return r, fmt.Errorf("Azure start returned no execution reference")
	}
	r.Refs = append(r.Refs, id)
	r.Status = compute.Accepted
	return r, nil
}
func (p *Provider) submitECS(ctx context.Context, l compute.Launch, pl plan) (compute.Submission, error) {
	r := compute.Submission{LaunchID: l.LaunchID, Status: compute.Unavailable}
	a := pl.Allocation
	arch := "X86_64"
	if a.Platform == "linux/arm64" {
		arch = "ARM64"
	}
	args := []string{"--timeout", fmt.Sprintf("%ds", pl.Request.TimeoutSeconds), "--"}
	args = append(args, l.Process.Command...)
	args = append(args, l.Process.Args...)
	container := map[string]any{"name": "executor", "image": a.Image, "essential": true, "entryPoint": []string{p.cfg.SupervisorPath}, "command": args, "environment": envList(l.Process, pl.Request.Metadata), "mountPoints": []map[string]any{{"sourceVolume": "scratch", "containerPath": "/scratch", "readOnly": false}}}
	if l.Process.WorkingDir != "" {
		container["workingDirectory"] = l.Process.WorkingDir
	}
	tags := []map[string]string{}
	for k, v := range pl.Request.Metadata {
		if len(k) > 128 || len(v) > 256 {
			r.Status = compute.Rejected
			return r, fmt.Errorf("ECS correlation metadata exceeds tag limits")
		}
		tags = append(tags, map[string]string{"key": k, "value": v})
	}
	body := map[string]any{"family": nameFor(l.LaunchID), "networkMode": "awsvpc", "requiresCompatibilities": []string{"FARGATE"}, "cpu": strconv.FormatInt(pl.CPUUnits, 10), "memory": strconv.FormatInt(a.MemoryBytes/Mi, 10), "runtimePlatform": map[string]string{"operatingSystemFamily": "LINUX", "cpuArchitecture": arch}, "containerDefinitions": []any{container}, "volumes": []map[string]any{{"name": "scratch"}}, "executionRoleArn": p.cfg.ExecutionRole, "tags": tags}
	if p.cfg.TaskRole != "" {
		body["taskRoleArn"] = p.cfg.TaskRole
	}
	if pl.DiskGiB > 20 {
		body["ephemeralStorage"] = map[string]int64{"sizeInGiB": pl.DiskGiB}
	}
	response, err := p.aws(ctx, "register-task-definition", body)
	if err != nil {
		return r, err
	}
	var def struct {
		ARN string `json:"taskDefinitionArn"`
	}
	json.Unmarshal(response["taskDefinition"], &def)
	if def.ARN == "" {
		return r, fmt.Errorf("ECS registration returned no definition")
	}
	r.Refs = []string{def.ARN}
	public := "DISABLED"
	if p.cfg.PublicIP {
		public = "ENABLED"
	}
	network := map[string]any{"subnets": p.cfg.Subnets, "assignPublicIp": public}
	if len(p.cfg.SecurityGroups) > 0 {
		network["securityGroups"] = p.cfg.SecurityGroups
	}
	response, err = p.aws(ctx, "run-task", map[string]any{"cluster": p.cfg.Cluster, "taskDefinition": def.ARN, "launchType": "FARGATE", "platformVersion": "1.4.0", "count": 1, "clientToken": l.LaunchID, "startedBy": nameFor(l.LaunchID), "networkConfiguration": map[string]any{"awsvpcConfiguration": network}, "tags": tags})
	r.Status = compute.Unknown
	if err != nil {
		return r, err
	}
	var tasks []struct {
		ARN string `json:"taskArn"`
	}
	json.Unmarshal(response["tasks"], &tasks)
	if len(tasks) > 0 {
		for _, task := range tasks {
			if task.ARN == "" {
				return r, fmt.Errorf("ECS task lacks ARN")
			}
			r.Refs = append(r.Refs, task.ARN)
		}
		r.Status = compute.Accepted
		return r, nil
	}
	var failures []struct {
		Reason string `json:"reason"`
	}
	json.Unmarshal(response["failures"], &failures)
	if len(failures) == 0 {
		return r, fmt.Errorf("ECS returned neither tasks nor failures")
	}
	r.Status = compute.Rejected
	for _, f := range failures {
		if strings.HasPrefix(f.Reason, "RESOURCE:") || strings.Contains(strings.ToLower(f.Reason), "capacity") {
			r.Status = compute.NoCapacity
		}
	}
	return r, fmt.Errorf("ECS declined task: %s", failures[0].Reason)
}

func (p *Provider) acquire(ctx context.Context) error {
	select {
	case p.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *Provider) release() { <-p.gate }
