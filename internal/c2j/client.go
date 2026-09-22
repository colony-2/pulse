// Package c2j provides embedded and external c2j listing and executor commands.
package c2j

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	"github.com/colony-2/cortex/internal/quantity"
	"github.com/colony-2/cortex/pkg/compute"
	"github.com/distribution/reference"
)

type Route struct {
	JobType  string `json:"jobType" yaml:"job_type"`
	TaskType string `json:"taskType,omitempty" yaml:"task_type,omitempty"`
}
type View = execution.View
type Job struct {
	Tenant          string    `json:"tenant_id"`
	ID              string    `json:"job_id"`
	Repository      string    `json:"repo,omitempty"`
	Status          string    `json:"status"`
	Next            *Route    `json:"next_route"`
	Execution       *View     `json:"execution"`
	CancelRequested bool      `json:"cancel_requested"`
	AvailableAt     time.Time `json:"available_at"`
}
type Page struct {
	Jobs []Job  `json:"jobs"`
	Next string `json:"next_page_token"`
}
type Client struct {
	Executable, WorkingDir, ExpectedVersion string
	Env                                     map[string]string
	Run                                     func(context.Context, []string) ([]byte, error)
}
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, fmt.Errorf("c2j output exceeds %d bytes", b.max)
	}
	return b.Buffer.Write(p)
}
func (c *Client) command(ctx context.Context, args []string) ([]byte, error) {
	if c.Run != nil {
		return c.Run(ctx, args)
	}
	cmd := exec.CommandContext(ctx, c.Executable, args...)
	cmd.Dir = c.WorkingDir
	cmd.WaitDelay = time.Second
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if !strings.HasPrefix(k, "C2J_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	for k, v := range c.Env {
		if strings.HasPrefix(k, "C2J_EXECUTION_") {
			return nil, fmt.Errorf("allocation variables cannot configure discovery")
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdout := &cappedBuffer{max: 16 << 20}
	stderr := &cappedBuffer{max: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("c2j %s failed: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}
func (c *Client) Check(ctx context.Context) error {
	if c.ExpectedVersion != "" {
		b, e := c.command(ctx, []string{"version"})
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(b)) != c.ExpectedVersion {
			return fmt.Errorf("c2j version differs from configured expected_version")
		}
	}
	b, e := c.command(ctx, []string{"run", "--help"})
	if e != nil {
		return e
	}
	if !strings.Contains(string(b), "--execution-memory") {
		return fmt.Errorf("c2j executable lacks execution allocation support")
	}
	return nil
}
func (c *Client) List(ctx context.Context, jobdb, cell, token string) (Page, error) {
	args := []string{"list", "--jobdb", jobdb, "--cell", cell, "--job-type", "recipe", "--status", "READY", "--status", "CRASH_CONCERN", "--page-size", "100", "--json"}
	if token != "" {
		args = append(args, "--page-token", token)
	}
	b, err := c.command(ctx, args)
	if err != nil {
		return Page{}, err
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(b, &raw); err != nil {
		return Page{}, fmt.Errorf("invalid c2j JSON: %w", err)
	}
	if len(raw["jobs"]) == 0 || string(raw["jobs"]) == "null" {
		return Page{}, fmt.Errorf("c2j JSON lacks jobs array")
	}
	var page Page
	if err = json.Unmarshal(b, &page); err != nil {
		return Page{}, err
	}
	return page, nil
}
func (j Job) Ready(routes []Route, now time.Time) bool {
	if j.CancelRequested || (j.Status != "READY" && j.Status != "CRASH_CONCERN") || j.Next == nil || j.AvailableAt.After(now) {
		return false
	}
	for _, r := range routes {
		if *j.Next == r {
			return true
		}
	}
	return false
}
func (j Job) Allocation(defaults compute.Allocation) (compute.Allocation, error) {
	if j.Tenant == "" || j.ID == "" {
		return defaults, fmt.Errorf("missing job identity")
	}
	v := j.Execution
	if v == nil {
		return defaults, fmt.Errorf("missing execution view")
	}
	switch v.Status {
	case "specified", "unspecified", "unresolved":
	default:
		return defaults, fmt.Errorf("execution status %q: %s", v.Status, v.Diagnostic)
	}
	if v.Diagnostic != "" {
		return defaults, fmt.Errorf("execution diagnostic: %s", v.Diagnostic)
	}
	if v.Demand == nil {
		if v.Status != "unresolved" {
			return defaults, fmt.Errorf("resolved execution view lacks demand")
		}
		return defaults, defaults.Validate()
	}
	if v.Demand.SchemaVersion != 1 {
		return defaults, fmt.Errorf("unsupported demand schema %d", v.Demand.SchemaVersion)
	}
	r := v.Demand.Effective
	a := defaults
	for _, field := range []struct {
		v    *string
		dest *int64
		cpu  bool
	}{{r.Resources.CPU, &a.CPUMillis, true}, {r.Resources.Memory, &a.MemoryBytes, false}, {r.Resources.EphemeralStorage, &a.ScratchBytes, false}} {
		if field.v != nil {
			n, e := quantity.Parse(*field.v, field.cpu)
			if e != nil {
				return a, e
			}
			*field.dest = n
		}
	}
	if r.Image != nil {
		a.Image = *r.Image
	}
	if r.Platform != nil {
		a.Platform = strings.ToLower(*r.Platform)
	}
	image, err := reference.ParseNormalizedNamed(a.Image)
	if err != nil {
		return a, err
	}
	a.Image = reference.TagNameOnly(image).String()
	// Requested identity is not evidence of an actual pulled image.
	a.ImageDigest = ""
	a.ImageID = ""
	return a, a.Validate()
}
func Process(jobdb, job, launch string, a compute.Allocation, extra map[string]string, metadata map[string]string) (compute.Process, error) {
	env := map[string]string{}
	for k, v := range extra {
		if strings.HasPrefix(k, "C2J_EXECUTION_") || k == "C2J_JOBDB" || strings.HasPrefix(k, "CORTEX_") {
			return compute.Process{}, fmt.Errorf("reserved executor environment key %s", k)
		}
		env[k] = v
	}
	env["C2J_EXECUTION_CPU"] = quantity.CPU(a.CPUMillis)
	env["C2J_EXECUTION_MEMORY"] = quantity.Bytes(a.MemoryBytes)
	env["C2J_EXECUTION_EPHEMERAL_STORAGE"] = quantity.Bytes(a.ScratchBytes)
	env["C2J_EXECUTION_PLATFORM"] = a.Platform
	env["C2J_EXECUTION_IMAGE"] = a.Image
	if a.ImageDigest != "" {
		env["C2J_EXECUTION_IMAGE_DIGEST"] = a.ImageDigest
	}
	if a.ImageID != "" {
		env["C2J_EXECUTION_IMAGE_ID"] = a.ImageID
	}
	for k, v := range metadata {
		env[strings.ToUpper(k)] = v
	}
	return compute.Process{Command: []string{"c2j"}, Args: []string{"run", "--jobdb", jobdb, "--job-id", job, "--worker-id", launch, "--on-not-ready", "fail", "--ci", "--input-mode", "fail"}, Env: env}, nil
}
