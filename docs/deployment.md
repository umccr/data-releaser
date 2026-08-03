# Deployment

This document covers deploying the **Infrastructure_Stack** — the AWS CDK
(TypeScript) application under `infrastructure/` that provisions the Data Control
account resources this project owns: the `ShareRecords` DynamoDB table, the two
Authorization Service Lambdas, the API Gateway REST API, their execution roles,
and the Manifest_Bucket with its cross-account read policy.

The stack deploys **nothing outside the Data Control account** — no
researcher-side resources, no Lake Formation / RAM sharing, and not htsget-rs
itself. Those are owned elsewhere (see Requirement 17 and the design document's
"Deployment ownership boundaries" table).

The `infrastructure/README.md` is the quick reference; this document is the
operator-facing walkthrough.

## Prerequisites

- **Node.js** and **npm** (the CDK app is TypeScript). It pins
  `aws-cdk`/`aws-cdk-lib` `2.160.0`.
- **Go** matching `go.mod` (`go 1.26.5`) — required to cross-compile the Lambda
  binaries.
- **AWS credentials** for the target Data Control account, resolvable through
  the standard chain (a profile, SSO, environment variables, or an assumed
  role) with permission to create the stack's resources. `cdk synth` works with
  no credentials; `cdk deploy` requires them.
- The account must be **CDK-bootstrapped** in the target region
  (`cdk bootstrap aws://<account>/<region>`) if it has not been already.

## Build the Lambda binaries first

Both Authorization Service Lambdas are Go binaries on the `provided.al2023`
custom runtime targeting **arm64/Graviton**. AWS custom runtimes require the
executable to be named `bootstrap`, and the stack references
`infrastructure/dist/authorization/` and `infrastructure/dist/admin/` as its code
assets. Those directories are git-ignored and must be (re)built before every
synth or deploy:

```sh
cd infrastructure
npm install
npm run build:lambdas   # runs scripts/build-lambdas.sh
```

Under the hood `build-lambdas.sh` cross-compiles each entrypoint statically:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o dist/<fn>/bootstrap ./functions/<fn>
```

for `<fn>` in `authorization` and `admin`.

## Synthesise and deploy

From the `infrastructure/` directory:

```sh
npm run build     # tsc typecheck/compile (optional but recommended)
npm test          # CDK assertion tests over the synthesised template

npx cdk synth     # synthesise the CloudFormation template (no credentials needed)
npx cdk deploy    # deploy to the target account/region
```

`cdk synth` needs no credentials as long as `account`/`region` match the VPC
lookup cached in `infrastructure/cdk.context.json` (checked in):
`npx cdk synth -c account=916218008751 -c region=ap-southeast-2`. A lookup for
a new account, region or `apiVpcId` needs credentials once to populate that
cache. `cdk deploy` always uses your resolved AWS credentials.

## Targeting an account and region

Everything that varies between environments is a **parameter** supplied through
CDK context (`-c key=value`), never hardcoded — so one source tree deploys to a
development and a production Data Control account with no code change. Defaults
live in `infrastructure/cdk.json` under the `dataReleaser` key; a per-invocation
`-c` flag overrides them.

`account` and `region` fall back to the standard `CDK_DEFAULT_ACCOUNT` /
`CDK_DEFAULT_REGION` environment variables (populated from the active AWS
profile) when not given explicitly.

```sh
# Deploy to the production Data Control account, granting two researcher
# accounts read on their own manifest prefix.
npm run build:lambdas
npx cdk deploy \
  -c account=916218008751 \
  -c region=ap-southeast-2 \
  -c researcherAccountIds=111111111111,222222222222
```

### Context parameters

| Context key | Meaning | Default |
| --- | --- | --- |
| `account` | Target AWS account ID | `CDK_DEFAULT_ACCOUNT`, else a placeholder |
| `region` | Target region | `CDK_DEFAULT_REGION`, else `ap-southeast-2` |
| `namePrefix` | Prefix applied to physical resource names | `data-releaser` |
| `stageName` | API Gateway REST stage name (also emitted as an output) | `prod` |
| `researcherAccountIds` | Accounts granted manifest read; JSON array or comma-separated list; may be empty | `[]` |
| `cacheTtlCeiling` | Cache_TTL ceiling, Go duration string | `1h` |
| `cacheDenyTtl` | Negative-cache max-age, Go duration string | `60s` |
| `encryption` | Manifest_Bucket SSE mode: `s3-managed` or `kms` | `s3-managed` |
| `provisionOperatorPolicy` | Provision the optional operator IAM policy (Req 11.2) | `false` |
| `apiVpcId` | VPC hosting the API's interface endpoint and all Lambdas; must be the VPC the htsget-rs Lambda runs in. **Required** — there is no public deployment mode | `vpc-03e01fb6290012661` |

Onboarding a researcher's manifest read access is a configuration change: add
their account ID to `researcherAccountIds` and redeploy. Each account gets a
bucket-policy statement granting `s3:GetObject` on `manifests/<account-id>/*`
only (plus a matching `kms:Decrypt` grant when `encryption=kms`).

> **The stack provisions no authorization cache.** Authorization responses are
> cached solely by the htsget-rs callout client, governed by the `Cache-Control`
> and `Vary` directives the Authorization Service Lambda emits — which is what
> `cacheTtlCeiling` and `cacheDenyTtl` configure. There is no API Gateway
> response cache, so there is no provisioned cache cluster to pay for and nothing
> to flush.

## Stack outputs

The stack exports five CloudFormation outputs. Three of them map directly onto
the CLI configuration fields that
reference deployed resources (see
[`docs/configuration.md`](configuration.md#mapping-to-infrastructure_stack-outputs)),
so those values are read from the deployment rather than guessed:

| Output | CLI config field | Notes |
| --- | --- | --- |
| `ApiGatewayRestApiId` | `endpoints.apiGatewayRestApiId` | REST API ID; consumed by the `validate` API Gateway stage check. |
| `ApiGatewayStageName` | `endpoints.apiGatewayStageName` | Stage name (equals `stageName`; default `prod`). |
| `ManifestBucketName` | `manifest.bucket` | Manifest_Bucket name (`<namePrefix>-manifests-<account>`). |
| `AuthorizationCalloutUrl` | `endpoints.authorization` | The callout URL (`<api-url>authorize`); append `?sourceVpcArn=...`. This is also the URL htsget-rs's `[auth.authorization]` callout config points at. Private DNS keeps the standard `execute-api` hostname — it simply does not resolve outside the VPC. |
| `AuthorizationApiVpcEndpointId` | — | The `execute-api` interface endpoint that is the sole route to the API; the resource policy denies everything with a different `aws:SourceVpce`. |

Because the API is private, `endpoints.authorization` is not reachable
from an operator's machine. Set `validation.skipAuthorizationCheck` so `validate`
reports that probe as `SKIP` instead of failing on a healthy deployment; see
[`infrastructure/README.md`](../infrastructure/README.md#api-authorization) for
the reasoning and [`RUNBOOK.md`](../DEMO.md) for how to verify the endpoint
from inside the VPC.

Read the outputs after deploying, for example:

```sh
aws cloudformation describe-stacks \
  --stack-name DataReleaserStack \
  --query "Stacks[0].Outputs" --output table
```

If `provisionOperatorPolicy=true`, the stack additionally exports
`OperatorPolicyArn` — the ARN of the optional operator IAM policy (Requirement
11.2). Provisioning this policy does not change the rule that the CLI itself
creates no IAM at runtime; it is deployment-time infrastructure attached to
operators out of band.

## After deployment: wire up and verify

1. Copy the three mapped outputs into your CLI config
   (`endpoints.apiGatewayRestApiId`, `endpoints.apiGatewayStageName`,
   `manifest.bucket`) — see [`docs/configuration.md`](configuration.md).
2. Run `data-releaser validate` to confirm the Data Control resources are
   present and consistent with configuration (the DynamoDB table, the
   Manifest_Bucket, and the API Gateway stage), alongside the other provider-side
   checks. See [`docs/usage.md`](usage.md#validate--validate-the-sharing-infrastructure).

## Redeploying and teardown

Redeploy with the same command after changing parameters or rebuilding the
Lambda binaries (`npm run build:lambdas` first). The DynamoDB table and
Manifest_Bucket use a `RETAIN` removal policy, so they are **not** destroyed when
the stack is deleted — expired and revoked Share_Records and researcher manifests
are audit data and must survive stack lifecycle operations. Removing them
requires a deliberate, separate action.
