# Cloud authentication

The standard `ghcr.io/colony-2/cortex` image supports Cloud Run Jobs, ECS Fargate, and Azure Container Apps Jobs directly. Cloud API clients and credential discovery are compiled into Cortex. No `aws`, `gcloud`, `az`, shell, or Node.js installation is required. The same behavior is available in the standalone executable.

Configure the provider's project/account, region, network, and other launch settings in [clouds.yaml](../examples/clouds.yaml). Supply credentials to the **Cortex controller**, either through its hosting environment or environment variables/mounted files. Native SDK credential caches refresh expiring credentials automatically during polling; no login command runs inside the container.

## Credential sources

| Provider | Running in the cloud | Running elsewhere |
| --- | --- | --- |
| Google Cloud | Application Default Credentials discovers the service account attached to the host/workload through Google's metadata service, including on Cloud Run and Compute Engine. | Set `GOOGLE_APPLICATION_CREDENTIALS` to a mounted service-account or workload-identity-federation JSON configuration. A mounted local ADC file is also supported. |
| AWS | The SDK discovers ECS task-role credentials, EC2 instance-profile credentials through IMDS, or a web-identity role configured by the platform. | Set `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN` for temporary credentials; or mount shared AWS configuration/credential files. |
| Azure | Use managed identity on the hosting service, or a federated workload identity supplied by the platform. | Set `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and `AZURE_CLIENT_SECRET`; certificate and federated-token files are also supported. |

Enable/attach the identity on the hosting platform and grant it permission to create and start the relevant job/task resources. Cortex does not create an identity or grant itself permissions. It needs network access to both the cloud API and the chosen token/metadata endpoints.

### Google Cloud

Cortex uses Google's [Application Default Credentials discovery](https://docs.cloud.google.com/go/docs/reference/cloud.google.com/go/auth/latest/credentials) with the `cloud-platform` scope. Explicit `GOOGLE_APPLICATION_CREDENTIALS` takes precedence over the well-known local ADC file and metadata credentials. An invalid explicitly selected file fails authentication.

For an off-cloud controller, mount the JSON file and set its **container path**:

```sh
docker run --rm --read-only --tmpfs /tmp \
  --mount "type=bind,source=$PWD/cortex.yaml,target=/etc/cortex/cortex.yaml,readonly" \
  --mount "type=bind,source=$PWD/google-credentials.json,target=/credentials/google.json,readonly" \
  -e GOOGLE_APPLICATION_CREDENTIALS=/credentials/google.json \
  ghcr.io/colony-2/cortex:latest
```

Federation can use a mounted subject-token file or a platform endpoint, as described in its credential configuration; make those paths/endpoints available inside the container. Configurations requiring an external credential executable need that executable and are unsuitable for the default image. A quota project in ADC, or `GOOGLE_CLOUD_QUOTA_PROJECT`, is sent as `X-Goog-User-Project` on Cloud Run requests.

In Google Cloud, attach an appropriate service account to the controller and omit explicit credentials to use metadata discovery. The provider's `service_account` setting selects the identity of the **launched executor**, not the controller's credentials. Give the controller permission to create/run jobs and act as that executor service account.

### AWS

Cortex uses the [AWS SDK for Go v2 credential chain](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html). This includes environment credentials, shared configuration, web identity, ECS container credentials, and EC2 instance profiles. ECS API calls use native request signing; credential refresh remains SDK-owned.

For environment credentials already exported in the host shell:

```sh
docker run --rm --read-only --tmpfs /tmp \
  --mount "type=bind,source=$PWD/cortex.yaml,target=/etc/cortex/cortex.yaml,readonly" \
  -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY -e AWS_SESSION_TOKEN \
  ghcr.io/colony-2/cortex:latest
```

For shared files, mount them read-only and set `AWS_SHARED_CREDENTIALS_FILE`, `AWS_CONFIG_FILE`, and optionally `AWS_PROFILE`. For web identity, supply `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` pointing to the platform-provided token. Credential profiles requiring `credential_process` need an external helper that the default image does not include; perform any interactive setup outside the container.

In ECS, attach a **task role to the controller task**; the controller's task execution role, used for image pulling/logging, is not the SDK's workload identity. On EC2, attach an instance profile and make IMDS reachable from the container; platform metadata/hop-limit settings can affect that access. On platforms using web identity, allow the platform to inject the role and projected token.

The provider's `execution_role` and `task_role` select roles for **new executor tasks**. Grant the controller ECS task-definition registration and task-launch permissions, plus `iam:PassRole` for those roles. Controller credentials are not copied into executor environment variables. Native launch requests have SDK retries disabled so an uncertain start is not retried invisibly.

### Azure

Cortex uses the [Azure Identity Go library](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity), selecting these production credential sources in order:

1. Environment service-principal credentials when `AZURE_CLIENT_SECRET` or `AZURE_CLIENT_CERTIFICATE_PATH` is set. Also set `AZURE_TENANT_ID` and `AZURE_CLIENT_ID`; certificates can use `AZURE_CLIENT_CERTIFICATE_PASSWORD`.
2. Workload identity when `AZURE_FEDERATED_TOKEN_FILE` is set, together with `AZURE_TENANT_ID` and `AZURE_CLIENT_ID`.
3. Managed identity supplied by the hosting platform. Set `AZURE_CLIENT_ID` to select a user-assigned identity, or omit it for the platform's default identity.

Explicitly configured but invalid credentials fail; Cortex does not switch to another identity after that failure. The chain does not invoke Azure CLI, PowerShell, or developer login tools. Tokens target Azure Resource Manager (`https://management.azure.com/.default`); the current adapter uses public Azure endpoints.

For service-principal variables exported in the host shell:

```sh
docker run --rm --read-only --tmpfs /tmp \
  --mount "type=bind,source=$PWD/cortex.yaml,target=/etc/cortex/cortex.yaml,readonly" \
  -e AZURE_TENANT_ID -e AZURE_CLIENT_ID -e AZURE_CLIENT_SECRET \
  ghcr.io/colony-2/cortex:latest
```

In Azure, enable managed identity on the controller's VM, Container App/Job, or other supported hosting service and grant it permission to read/create/start jobs in the target resource group. Keep platform-injected identity endpoint/header variables available. These authenticate the controller; executor identity and image-pull permissions are separate deployment concerns.

## Container configuration and lifecycle

Files must be readable by the image's UID/GID `65532:65532`; environment file paths refer to paths inside the container. Use read-only mounts and a writable `/tmp` as shown above. Refreshing metadata, service-account, service-principal, and workload-identity credentials does not require rewriting mounted credential files. A platform rotating a projected token must keep that file available at its configured path. Fixed temporary AWS credentials supplied directly as environment variables cannot be renewed by the SDK; use a refreshable role/profile source for long-running deployments or restart with replacement values.

For Google and Azure, existing `providers.<name>.token_env` remains an explicit access-token override. If set, it bypasses SDK credential discovery. The supplied token must be nonempty and valid; Cortex cannot refresh a raw access token. Omit this setting to use automatic SDK authentication. ECS uses AWS credentials and signing, not bearer `token_env`.

`cortex -check` validates configuration and initializes providers, but cloud credentials are acquired lazily on submission. Use `-once` with a runnable test job to verify authentication and launch permissions. Authentication failures before launching return `unavailable`; uncertain start responses retain the scheduler's normal cooldown behavior.

The Cortex image is the controller. Executor images selected by `defaults.image` or job demand still need c2j and recipe dependencies. Running this controller image unchanged does not install those dependencies in arbitrary executor images.
