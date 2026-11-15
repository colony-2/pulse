// Package c2j provides c2j library listing, job projections, and executor commands.
package c2j

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/colony-2/c2j/pkg/execution"
	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/bases"
	"github.com/colony-2/pulse/internal/placement"
	"github.com/colony-2/pulse/internal/providerutil"
	"github.com/colony-2/pulse/internal/quantity"
	"github.com/colony-2/pulse/pkg/compute"
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
	// Metadata retains immutable submission data, including parsing errors, for
	// interpreting a genuinely absent demand in the acquired lease.
	Metadata json.RawMessage `json:"-"`
}
type Page struct {
	Jobs []Job  `json:"jobs"`
	Next string `json:"next_page_token"`
}

func (j Job) Allocation(defaults provider.Allocation) (provider.Allocation, error) {
	a, _, err := j.AllocationWithDiagnostic(defaults)
	return a, err
}

// AllocationWithDiagnostic captures facts during the existing translation so
// partial knowledge survives a later preparation error without allowing launch.
func (j Job) AllocationWithDiagnostic(defaults provider.Allocation) (a provider.Allocation, snapshot placement.Requested, allocationErr error) {
	snapshot = placement.UnknownRequest("leased allocation unavailable")
	a, allocationErr = j.allocation(defaults, &snapshot)
	return a, snapshot, allocationErr
}

func (j Job) allocation(defaults provider.Allocation, snapshot *placement.Requested) (provider.Allocation, error) {
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
		*snapshot = placement.Allocation(defaults)
		return defaults, providerutil.Allocation(defaults)
	}
	if err := v.Demand.Validate(); err != nil {
		return defaults, fmt.Errorf("invalid execution demand: %w", err)
	}
	r := v.Demand.Effective
	a := defaults
	*snapshot = placement.Allocation(defaults)
	if r.Base != nil || r.Image != nil || r.Platform != nil {
		snapshot.Base = placement.Unknown("effective base translation incomplete")
		snapshot.Platform = placement.Unknown("effective base translation incomplete")
	}
	if r.Platform != nil {
		snapshot.Platform = placement.Known(strings.ToLower(*r.Platform))
	}
	snapshot.Capabilities = placement.Known(append(append([]string{}, defaults.Capabilities...), r.Capabilities...))
	// Valid leased quantities are positive. The provider's zero denotes an absent
	// constraint, so no explicit-zero presence can be lost in this translation.
	for _, field := range []struct {
		source *string
		fact   *placement.Fact
	}{{r.Resources.CPU, &snapshot.CPUMillis}, {r.Resources.Memory, &snapshot.MemoryBytes}, {r.Resources.EphemeralStorage, &snapshot.ScratchBytes}} {
		if field.source != nil {
			*field.fact = placement.Unknown("resource translation incomplete")
		}
	}
	for _, field := range []struct {
		v    *string
		dest *int64
		cpu  bool
		fact *placement.Fact
	}{{r.Resources.CPU, &a.CPUMillis, true, &snapshot.CPUMillis}, {r.Resources.Memory, &a.MemoryBytes, false, &snapshot.MemoryBytes}, {r.Resources.EphemeralStorage, &a.ScratchBytes, false, &snapshot.ScratchBytes}} {
		if field.v != nil {
			n, e := quantity.Parse(*field.v, field.cpu)
			if e != nil {
				return a, e
			}
			*field.dest = n
			*field.fact = placement.Known(n)
		}
	}
	a.Capabilities = append(append([]string(nil), defaults.Capabilities...), r.Capabilities...)
	if r.Base != nil {
		a.Base = *r.Base
	} else if r.Image != nil {
		platform := "linux/arm64"
		if r.Platform != nil {
			platform = strings.ToLower(*r.Platform)
		} else if b, err := bases.Resolve(defaults.Base, nil); err == nil {
			platform = b.Platform
		}
		var err error
		a.Base, err = bases.OCI(*r.Image, platform)
		if err != nil {
			return a, err
		}
	} else if r.Platform != nil {
		platform := strings.ToLower(*r.Platform)
		selector := a.Base
		// An explicit platform overrides the architecture in the built-in default.
		if selector == bases.Default {
			selector = "ubuntu-26*"
		}
		var err error
		a.Base, err = bases.Constrain(selector, platform)
		if err != nil {
			return a, err
		}
	}
	*snapshot = placement.Allocation(a)
	if r.Platform != nil {
		snapshot.Platform = placement.Known(strings.ToLower(*r.Platform))
	}
	return a, providerutil.Allocation(a)
}
func Process(jobdb, job string, a compute.Allocation, extra map[string]string, metadata map[string]string) (compute.Process, error) {
	env := map[string]string{}
	for k, v := range extra {
		if strings.HasPrefix(k, "C2J_EXECUTION_") || k == "C2J_JOBDB" || strings.HasPrefix(k, "PULSE_") {
			return compute.Process{}, fmt.Errorf("reserved executor environment key %s", k)
		}
		env[k] = v
	}
	if a.CPUMillis > 0 {
		env["C2J_EXECUTION_CPU"] = quantity.CPU(a.CPUMillis)
	}
	if a.MemoryBytes > 0 {
		env["C2J_EXECUTION_MEMORY"] = quantity.Bytes(a.MemoryBytes)
	}
	if a.ScratchBytes > 0 {
		env["C2J_EXECUTION_EPHEMERAL_STORAGE"] = quantity.Bytes(a.ScratchBytes)
	}
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
	return compute.Process{Command: []string{"c2j"}, Args: []string{"run", "with-lease", "--jobdb", jobdb, "--job-id", job, "--lease-file", "-", "--ci", "--input-mode", "fail"}, Env: env}, nil
}
