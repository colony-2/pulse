package cloud

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
)

// Keep the SDK client and its refreshable credential cache for the lifetime of
// the provider. Native launch calls get one attempt: an ambiguous response must
// remain unknown rather than causing a hidden retry.
func (p *Provider) aws(ctx context.Context, action string, body any) (map[string]json.RawMessage, error) {
	if p.ecs == nil {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(p.cfg.Region), config.WithHTTPClient(p.HTTP))
		if err != nil {
			return nil, err
		}
		p.ecs = ecs.NewFromConfig(cfg, func(o *ecs.Options) {
			o.Retryer = aws.NopRetryer{}
			if p.BaseURL != "" {
				o.BaseEndpoint = aws.String(p.BaseURL)
			}
		})
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	// Translate the adapter's native request into SDK types; the SDK owns wire
	// serialization, endpoint selection, signing, and credential refresh.
	var response map[string]any
	switch action {
	case "register-task-definition":
		var input ecs.RegisterTaskDefinitionInput
		if err = json.Unmarshal(data, &input); err != nil {
			return nil, err
		}
		out, err := p.ecs.RegisterTaskDefinition(ctx, &input)
		if err != nil {
			return nil, err
		}
		response = map[string]any{"taskDefinition": out.TaskDefinition}
	case "run-task":
		var input ecs.RunTaskInput
		if err = json.Unmarshal(data, &input); err != nil {
			return nil, err
		}
		out, err := p.ecs.RunTask(ctx, &input)
		if err != nil {
			return nil, err
		}
		response = map[string]any{"tasks": out.Tasks, "failures": out.Failures}
	default:
		return nil, fmt.Errorf("unsupported ECS operation %q", action)
	}
	result := map[string]json.RawMessage{}
	for key, value := range response {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		result[key] = data
	}
	return result, nil
}
