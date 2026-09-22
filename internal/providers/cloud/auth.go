package cloud

import (
	"context"
	"fmt"
	"os"

	"cloud.google.com/go/auth/credentials"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// token is called under the provider's submission lock. Keep SDK credential
// instances across batches for caching/refresh, but use each call's context.
func (p *Provider) token(ctx context.Context) (string, error) {
	if p.cfg.TokenEnv != "" {
		token := os.Getenv(p.cfg.TokenEnv)
		if token == "" {
			return "", fmt.Errorf("missing cloud token environment")
		}
		return token, nil
	}
	if p.accessToken == nil {
		switch p.cfg.Kind {
		case "cloudrun":
			cred, err := credentials.DetectDefault(&credentials.DetectOptions{
				Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
				Client: p.HTTP, DisableAsyncRefresh: true,
			})
			if err != nil {
				return "", err
			}
			quota, err := cred.QuotaProjectID(ctx)
			if err != nil {
				return "", err
			}
			p.quotaProject = quota
			p.accessToken = func(ctx context.Context) (string, error) {
				token, err := cred.Token(ctx)
				if err != nil {
					return "", err
				}
				return token.Value, nil
			}
		case "azurejobs":
			cred, err := azureCredential(azcore.ClientOptions{Transport: p.HTTP})
			if err != nil {
				return "", err
			}
			p.accessToken = func(ctx context.Context) (string, error) {
				token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
				return token.Token, err
			}
		default:
			return "", fmt.Errorf("provider does not use bearer authentication")
		}
	}
	token, err := p.accessToken(ctx)
	if err == nil && token == "" {
		err = fmt.Errorf("empty cloud token")
	}
	return token, err
}

// Use the SDK's production credentials without developer-tool subprocesses.
// An explicitly configured but invalid source must fail instead of silently
// switching to a different identity.
func azureCredential(options azcore.ClientOptions) (azcore.TokenCredential, error) {
	if os.Getenv("AZURE_CLIENT_SECRET") != "" || os.Getenv("AZURE_CLIENT_CERTIFICATE_PATH") != "" {
		return azidentity.NewEnvironmentCredential(&azidentity.EnvironmentCredentialOptions{ClientOptions: options})
	}
	if os.Getenv("AZURE_FEDERATED_TOKEN_FILE") != "" {
		return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{ClientOptions: options})
	}
	managed := azidentity.ManagedIdentityCredentialOptions{ClientOptions: options}
	if id := os.Getenv("AZURE_CLIENT_ID"); id != "" {
		managed.ID = azidentity.ClientID(id)
	}
	return azidentity.NewManagedIdentityCredential(&managed)
}
