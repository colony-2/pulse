// Package plugin connects Pulse's registry to the public provider SDK.
package plugin

import (
	"context"
	"os"
	"time"

	provider "github.com/colony-2/pulse-provider-api"
	sdk "github.com/colony-2/pulse-provider-api/plugin"
	"github.com/colony-2/pulse/internal/placement"
	"github.com/colony-2/pulse/pkg/providerplugin"
)

func Start(ctx context.Context, options providerplugin.ProcessConfig, executorEnv map[string]string) (*Process, error) {
	env := make(map[string]string, len(options.PassEnv))
	for _, key := range options.PassEnv {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	process, err := sdk.Start(ctx, sdk.StartOptions{
		Executable: options.Command, Args: options.Args, Env: env, SHA256: options.SHA256,
		Init: sdk.Initialization{
			Name: options.Name, BaseDir: options.BaseDir, ConfigYAML: options.ConfigYAML, ExecutorEnv: executorEnv,
		},
		StartupTimeout: options.StartTimeout, CallTimeout: options.CallTimeout, ShutdownTimeout: options.ShutdownTimeout,
		Concurrency: options.MaxConcurrent, MaxMessageBytes: options.MaxMessageBytes,
		ProbeInterval: 5 * time.Second, ProbeTimeout: time.Second,
		Restart: sdk.RestartPolicy{MaxAttempts: 3, MaxDelay: 4 * time.Second, JitterFraction: 0.25, HealthyResetInterval: 5 * time.Minute},
	})
	if err != nil {
		return nil, err
	}
	return &Process{process}, nil
}

type Process struct{ *sdk.Process }

var _ provider.Provider = (*Process)(nil)

func (p *Process) SubmitWithDiagnostics(ctx context.Context, launches []provider.Launch) ([]provider.Submission, map[string]placement.SubmissionDetail, error) {
	results, observations, err := p.SubmitDetailed(ctx, launches)
	methodError := placement.Error(err)
	if observations.MethodError.Present {
		message := observations.MethodError.Text
		if observations.MethodError.Truncated {
			message += "\n[plugin method error truncated at the SDK observation limit]"
		}
		methodError = &message
	}
	var captureError *string
	if observations.Capture != "complete" {
		message := "original plugin results were not captured"
		if observations.Capture == "truncated" {
			message = "original plugin results truncated at the SDK observation limit"
		}
		captureError = &message
	}
	details := make(map[string]placement.SubmissionDetail, len(launches))
	for _, launch := range launches {
		detail := placement.SubmissionDetail{
			OriginalResults: []provider.Submission{}, Error: methodError, ErrorScope: "batch", OriginalResultsError: captureError,
		}
		for _, outcome := range observations.Outcomes {
			if outcome.LaunchID == launch.LaunchID {
				detail.OriginalResults = append(detail.OriginalResults, provider.Submission{
					LaunchID: outcome.LaunchID, Status: provider.Status(outcome.Status), Reason: outcome.Reason,
				})
			}
		}
		details[launch.LaunchID] = detail
	}
	return results, details, err
}
