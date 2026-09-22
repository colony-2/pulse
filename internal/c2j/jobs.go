// Package c2j provides c2j library listing, job projections, and executor commands.
package c2j

import (
	"fmt"
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
	// The provider must enforce this image constraint before starting the process,
	// just as it must guarantee the requested resources advertised above.
	if _, pinnedDigest, pinned := strings.Cut(a.Image, "@"); pinned {
		env["C2J_EXECUTION_IMAGE_DIGEST"] = pinnedDigest
	}
	for k, v := range metadata {
		env[strings.ToUpper(k)] = v
	}
	return compute.Process{Command: []string{"c2j"}, Args: []string{"run", "--jobdb", jobdb, "--job-id", job, "--worker-id", launch, "--on-not-ready", "fail", "--ci", "--input-mode", "fail"}, Env: env}, nil
}
