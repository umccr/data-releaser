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

- **Node.js** and **pnpm** (the CDK app is TypeScript). The package manager is
  pinned via `packageManager` in `infrastructure/package.json`, so
  `corepack enable` is enough to get the right pnpm version. It pins
  `aws-cdk`/`aws-cdk-lib` `2.160.0`.
- **Go** matching `go.mod` (`go 1.26.5`) — required to cross-compile the Lambda
  binaries.
- **AWS credentials** for the target Data Control account, resolvable through
  the standard chain (a profile, SSO, environment variables, or an assumed
  role) with permission to create the stack's resources. `cdk synth` works with
  no credentials; `cdk deploy` requires them.
- The account must be **CDK-bootstrapped** in the target region
  (`cdk bootstrap aws://<account>/<region>`) if it has not been already.

## The Lambda binaries build themselves

Both Authorization Service Lambdas are Go binaries on the `provided.al2023`
custom runtime targeting **arm64/Graviton**. There is no separate build step:
the stack uses `GoFunction` from `@aws-cdk/aws-lambda-go-alpha`, which compiles
each entrypoint during `cdk synth` and names the output `bootstrap` as the
custom runtime requires.

```sh
cd infrastructure
pnpm install
```

The equivalent of what it runs per function, for `<fn>` in `authorization` and
`admin`:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o bootstrap ./functions/<fn>
```

`GOOS`, `GOARCH`, and `CGO_ENABLED=0` come from the construct (`GOARCH` follows
the function's `architecture`); `-trimpath -ldflags "-s -w"` is set via
`bundling.goBuildFlags` in `lib/data-releaser-stack.ts`. It builds with the
local Go toolchain when one is on `PATH`, and falls back to a Docker container
otherwise — so a machine with Go installed needs no Docker, and a machine with
neither Go nor Docker cannot synthesise.

Because the asset hash is derived from the compiled binary, a Go source change
is picked up automatically on the next synth. There is no build step to forget
and no stale binary to deploy.

## Synthesise and deploy

From the `infrastructure/` directory:

```sh
pnpm run build     # tsc typecheck/compile (optional but recommended)
pnpm test          # CDK assertion tests over the synthesised template

pnpm exec cdk synth     # synthesise the CloudFormation template (no credentials needed)
pnpm exec cdk deploy    # deploy to the target account/region
```

Neither command takes any `-c` flags: all configuration is read from
`infrastructure/cdk.context.json` (see below). `cdk synth` needs no credentials
as long as `account`/`region` there match the VPC lookup cached in that same
file (checked in). A lookup for a new account, region or `apiVpcId` needs
credentials once to populate that cache. `cdk deploy` always uses your resolved
AWS credentials.

## Targeting an account and region

Everything that varies between environments is a **parameter**, never hardcoded
— so one source tree deploys to a development and a production Data Control
account with no code change. Every parameter lives under the `dataReleaser` key
of `infrastructure/cdk.context.json`; edit that file to retarget a deployment.

```jsonc
// infrastructure/cdk.context.json
{
  "dataReleaser": {
    "account": "916218008751",
    "region": "ap-southeast-2",
    "apiVpcId": "vpc-03e01fb6290012661",
    "researcherAccountIds": ["111111111111", "222222222222"]
    // ... see the table below
  },
  // The VPC lookup cache CDK maintains itself — leave it alone.
  "vpc-provider:account=916218008751:filter.vpc-id=...": { }
}
```

`account` and `region` may be omitted, in which case they fall back to the
standard `CDK_DEFAULT_ACCOUNT` / `CDK_DEFAULT_REGION` environment variables
(populated from the active AWS profile).

Two things to know about this file rather than `cdk.json` or `-c` flags:

- **`-c` cannot carry non-strings.** `cdk`'s own help calls `-c` a "contextual
  *string* parameter", and that is literal: `-c provisionOperatorPolicy=false`
  arrives in the app as the string `"false"`, which is truthy. A command-line
  interface therefore needs a coercion layer on every boolean, number and list
  field, and a bug in that layer silently deploys the opposite of what was
  asked. `cdk.context.json` is parsed as JSON, so values arrive with their real
  types and are used directly. `-c` flags are not read at all.
- **`cdk.json` context wins over `cdk.context.json` for the same key**, so no
  `dataReleaser` block may be added back to `cdk.json` — it would silently
  override this file. `cdk.json` holds only CDK feature flags.

> ⚠️ **`cdk context --clear` and `cdk context --reset` wipe this configuration.**
> Those commands are meant for the lookup cache, but they rewrite the whole file
> and delete the hand-authored `dataReleaser` block along with it. If you run
> one to refresh a stale VPC lookup, restore the block afterwards with
> `git checkout infrastructure/cdk.context.json`, or delete just the
> `vpc-provider:` entry by hand instead.

### Context parameters

| Context key | Meaning | Default |
| --- | --- | --- |
| `account` | Target AWS account ID | `CDK_DEFAULT_ACCOUNT`, else a placeholder |
| `region` | Target region | `CDK_DEFAULT_REGION`, else `ap-southeast-2` |
| `namePrefix` | Prefix applied to physical resource names | `data-releaser` |
| `stageName` | API Gateway REST stage name (also emitted as an output) | `prod` |
| `researcherAccountIds` | Accounts granted manifest read; JSON array of account-ID strings, may be empty | `[]` |
| `cacheTtlCeiling` | Cache_TTL ceiling, Go duration string | `1h` |
| `cacheDenyTtl` | Negative-cache max-age, Go duration string | `60s` |
| `encryption` | Manifest_Bucket SSE mode: `s3-managed` or `kms` | `s3-managed` |
| `provisionOperatorPolicy` | Provision the optional operator IAM policy (Req 11.2) | `false` |
| `apiVpcId` | VPC hosting the API's interface endpoint and all Lambdas; must be the VPC the htsget-rs Lambda runs in. **Required** — there is no public deployment mode | `vpc-03e01fb6290012661` |

Onboarding a researcher's manifest read access is a configuration change: add
their account ID to `dataReleaser.researcherAccountIds` in `cdk.context.json`
and redeploy. Each account gets a
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

Redeploy with the same command after changing parameters or the Lambda Go
sources; synth recompiles the binaries. The DynamoDB table and
Manifest_Bucket use a `RETAIN` removal policy, so they are **not** destroyed when
the stack is deleted — expired and revoked Share_Records and researcher manifests
are audit data and must survive stack lifecycle operations. Removing them
requires a deliberate, separate action.
