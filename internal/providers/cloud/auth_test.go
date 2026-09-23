package cloud

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
)

func isolateCredentials(t *testing.T) {
	t.Helper()
	for _, key := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_QUOTA_PROJECT", "AZURE_CLIENT_SECRET", "AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_FEDERATED_TOKEN_FILE", "AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_AUTHORITY_HOST", "IDENTITY_ENDPOINT", "IDENTITY_HEADER", "MSI_ENDPOINT", "MSI_SECRET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("PATH", t.TempDir()) // All tested authentication must work without CLIs.
}

func TestGoogleServiceAccountRefresh(t *testing.T) {
	isolateCredentials(t)
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.Form.Get("assertion") == "" {
			t.Error("missing service account assertion")
		}
		// A short-lived first token forces the next call through synchronous refresh.
		expiry := 3600
		if calls == 1 {
			expiry = 1
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("google-%d", calls), "token_type": "Bearer", "expires_in": expiry})
	}))
	defer s.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	data, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "pulse@example.iam.gserviceaccount.com", "private_key": string(encoded), "private_key_id": "test-key", "token_uri": s.URL, "quota_project_id": "billing-project"})
	file := filepath.Join(t.TempDir(), "credentials.json")
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", file)
	p, _ := New(Config{Kind: "cloudrun", Project: "p", Region: "r"})
	ctx, cancel := context.WithCancel(context.Background())
	first, err := p.token(ctx)
	cancel()
	if err != nil || first != "google-1" {
		t.Fatal(first, err)
	}
	time.Sleep(1100 * time.Millisecond) // Let the first token expire.
	second, err := p.token(context.Background())
	if err != nil || second != "google-2" || calls != 2 || p.quotaProject != "billing-project" {
		t.Fatal(second, err, calls, p.quotaProject)
	}
	third, err := p.token(context.Background())
	if err != nil || third != second || calls != 2 {
		t.Fatal("credential cache not reused", third, err, calls)
	}
}

func TestGoogleMetadataIdentity(t *testing.T) {
	isolateCredentials(t)
	// Avoid consulting a developer's existing ADC file in this metadata test.
	if dir, err := os.UserHomeDir(); err == nil {
		if _, err = os.Stat(filepath.Join(dir, ".config/gcloud/application_default_credentials.json")); err == nil {
			t.Skip("local ADC file takes precedence over metadata")
		}
	}
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Error("metadata header missing")
		}
		w.Header().Set("Metadata-Flavor", "Google")
		if strings.HasSuffix(r.URL.Path, "/token") {
			calls++
			w.Write([]byte(`{"access_token":"metadata-token","token_type":"Bearer","expires_in":3600}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/universe/universe-domain") {
			w.Write([]byte("googleapis.com"))
			return
		}
		w.WriteHeader(404)
	}))
	defer s.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(s.URL, "http://"))
	p, _ := New(Config{Kind: "cloudrun", Project: "p", Region: "r"})
	token, err := p.token(context.Background())
	if err != nil || token != "metadata-token" || calls != 1 {
		t.Fatal(token, err, calls)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestAzureEnvironmentAndWorkloadIdentity(t *testing.T) {
	for _, federated := range []bool{false, true} {
		t.Run(fmt.Sprint(federated), func(t *testing.T) {
			isolateCredentials(t)
			t.Setenv("AZURE_TENANT_ID", "tenant")
			t.Setenv("AZURE_CLIENT_ID", "client")
			if federated {
				file := filepath.Join(t.TempDir(), "token")
				os.WriteFile(file, []byte("federated-assertion"), 0600)
				t.Setenv("AZURE_FEDERATED_TOKEN_FILE", file)
			} else {
				t.Setenv("AZURE_CLIENT_SECRET", "client-secret")
			}
			calls := 0
			p, _ := New(Config{Kind: "azurejobs", Region: "r", Subscription: "s", ResourceGroup: "g", EnvironmentID: "e"})
			p.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(r.URL.Path, "discovery/instance"):
					return jsonResponse(r, `{"tenant_discovery_endpoint":"https://login.microsoftonline.com/tenant/v2.0/.well-known/openid-configuration","metadata":[{"preferred_network":"login.microsoftonline.com","preferred_cache":"login.microsoftonline.com","aliases":["login.microsoftonline.com"]}]}`), nil
				case strings.Contains(r.URL.Path, "openid-configuration"):
					return jsonResponse(r, `{"authorization_endpoint":"https://login.microsoftonline.com/tenant/oauth2/v2.0/authorize","token_endpoint":"https://login.microsoftonline.com/tenant/oauth2/v2.0/token","issuer":"https://login.microsoftonline.com/tenant/v2.0"}`), nil
				case strings.HasSuffix(r.URL.Path, "/token"):
					calls++
					r.ParseForm()
					if !strings.Contains(r.Form.Get("scope"), "https://management.azure.com/.default") || r.Form.Get("client_id") != "client" {
						t.Error(r.Form)
					}
					if federated && r.Form.Get("client_assertion") != "federated-assertion" {
						t.Error("missing federation assertion")
					}
					if !federated && r.Form.Get("client_secret") != "client-secret" {
						t.Error("missing environment credential")
					}
					return jsonResponse(r, `{"access_token":"azure-token","token_type":"Bearer","expires_in":3600}`), nil
				default:
					return nil, fmt.Errorf("unexpected auth request %s", r.URL)
				}
			})}
			ctx, cancel := context.WithCancel(context.Background())
			token, err := p.token(ctx)
			cancel()
			if err != nil || token != "azure-token" {
				t.Fatal(token, err)
			}
			token, err = p.token(context.Background())
			if err != nil || token != "azure-token" || calls != 1 {
				t.Fatal("credential cache not reused", token, err, calls)
			}
		})
	}
}

func TestAzureManagedIdentity(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("AZURE_CLIENT_ID", "assigned-client")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-IDENTITY-HEADER") != "identity-secret" || r.URL.Query().Get("client_id") != "assigned-client" || r.URL.Query().Get("resource") != "https://management.azure.com" {
			t.Error(r.Header, r.URL)
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "managed-token", "expires_on": fmt.Sprint(time.Now().Add(time.Hour).Unix()), "token_type": "Bearer", "resource": "https://management.azure.com"})
	}))
	defer s.Close()
	t.Setenv("IDENTITY_ENDPOINT", s.URL)
	t.Setenv("IDENTITY_HEADER", "identity-secret")
	p, _ := New(Config{Kind: "azurejobs", Region: "r", Subscription: "s", ResourceGroup: "g", EnvironmentID: "e"})
	token, err := p.token(context.Background())
	if err != nil || token != "managed-token" {
		t.Fatal(token, err)
	}
}

func TestExplicitCredentialFailureDoesNotFallBack(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("AZURE_CLIENT_SECRET", "incomplete")
	p, _ := New(Config{Kind: "azurejobs", Region: "r", Subscription: "s", ResourceGroup: "g", EnvironmentID: "e"})
	if _, err := p.token(context.Background()); err == nil {
		t.Fatal("invalid environment silently ignored")
	}
	for _, kind := range []string{"cloudrun", "azurejobs"} {
		p := &Provider{cfg: Config{Kind: kind, TokenEnv: "PULSE_TEST_TOKEN"}, accessToken: func(context.Context) (string, error) { t.Fatal("override ignored"); return "", nil }}
		t.Setenv("PULSE_TEST_TOKEN", "")
		if _, err := p.token(context.Background()); err == nil {
			t.Fatal("empty explicit token accepted")
		}
		t.Setenv("PULSE_TEST_TOKEN", "explicit")
		if token, err := p.token(context.Background()); err != nil || token != "explicit" {
			t.Fatal(token, err)
		}
	}
}

func TestAWSRoleCredentialsAndNoLaunchRetry(t *testing.T) {
	for _, instance := range []bool{false, true} {
		t.Run(fmt.Sprint(instance), func(t *testing.T) {
			isolateCredentials(t)
			credentialCalls, starts := 0, 0
			metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if instance {
					if r.Method == "PUT" && r.URL.Path == "/latest/api/token" {
						w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
						w.Write([]byte("imds-token"))
						return
					}
					if r.Header.Get("X-Aws-Ec2-Metadata-Token") != "imds-token" {
						t.Error("missing IMDSv2 token")
					}
					if r.URL.Path == "/latest/meta-data/iam/security-credentials/" {
						w.Write([]byte("instance-role"))
						return
					}
				} else if r.Header.Get("Authorization") != "container-auth" {
					t.Error("missing task-role authorization")
				}
				credentialCalls++
				json.NewEncoder(w).Encode(map[string]any{"Code": "Success", "AccessKeyId": "role-key", "SecretAccessKey": "role-secret", "Token": "role-session", "Expiration": time.Now().Add(time.Hour).Format(time.RFC3339)})
			}))
			defer metadata.Close()
			if instance {
				t.Setenv("AWS_EC2_METADATA_DISABLED", "false")
				t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", metadata.URL)
			} else {
				t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", metadata.URL)
				t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN", "container-auth")
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.Header.Get("Authorization"), "Credential=role-key/") || r.Header.Get("X-Amz-Security-Token") != "role-session" {
					t.Error("missing role-based signature")
				}
				w.Header().Set("Content-Type", "application/x-amz-json-1.1")
				if strings.HasSuffix(r.Header.Get("X-Amz-Target"), "RegisterTaskDefinition") {
					w.Write([]byte(`{"taskDefinition":{"taskDefinitionArn":"arn:def"}}`))
					return
				}
				starts++
				w.WriteHeader(503)
				w.Write([]byte(`{"__type":"ServerException","message":"lost outcome"}`))
			}))
			defer api.Close()
			req := request("role")
			p, _ := New(Config{Kind: "ecs", Region: "us-east-1", Cluster: "cluster", ExecutionRole: "execution-role", Subnets: []string{"subnet"}, SupervisorPath: "/helper", ImageStorageBounds: map[string]int64{req.Image: Gi}})
			p.BaseURL = api.URL
			p.HTTP = api.Client()
			result, err := p.Submit(context.Background(), []compute.Launch{launch(req)})
			if err != nil || len(result) != 1 || result[0].Status != compute.Unknown || starts != 1 || credentialCalls != 1 {
				t.Fatal(result, err, starts, credentialCalls)
			}
		})
	}
}
