# Deployment

This document covers deploying the CDK (TypeScript) application under
`infrastructure/`, which provisions the Data Control account resources this
project owns. It holds two kinds of stack:

- **`DataReleaserStack`** — the `ShareRecords` DynamoDB table, the authorization
  and admin Lambdas, the copy completion Lambda when a copy orchestration is
  configured, the API Gateway REST API, their execution roles, and the manifest
  bucket with its cross-account read policy.
- **`HotCache-<name>`** — one stack per dataset released as direct S3 access,
  holding that dataset's cache bucket and the KMS key encrypting it, and nothing
  else. Each is deployed by name, on its own, and neither it nor
  `DataReleaserStack` depends on the other.

The app deploys **nothing outside the Data Control account** — no
researcher-side resources, no Lake Formation / RAM sharing, not htsget-rs, and
not the copy orchestration that moves a dataset into a cache bucket. Those are
owned elsewhere; [`docs/prerequisites.md`](prerequisites.md) lists what each
account has to have in place and who owns it.

The [`infrastructure/README.md`](../infrastructure/README.md) is the quick
reference; this document is the operator-facing walkthrough.

## Toolchain and credentials

- **Node.js** and **pnpm** (the CDK app is TypeScript). The package manager is
  pinned via `packageManager` in `infrastructure/package.json` (`pnpm@11.21.0`),
  so `corepack enable` is enough to get the right pnpm version. It pins
  `aws-cdk`/`aws-cdk-lib` `2.160.0` and `@aws-cdk/aws-lambda-go-alpha`
  `2.160.0-alpha.0`; because the Go bundling module is an alpha whose API sits
  outside semver, the two must be bumped together.
- **Go** matching `go.mod` (`go 1.26.5`) — required to cross-compile the Lambda
  binaries.
- **AWS credentials** for the target Data Control account, resolvable through
  the standard chain (a profile, SSO, environment variables, or an assumed
  role) with permission to create the stack's resources. `cdk synth` works with
  no credentials; `cdk deploy` requires them.
- The account must be **CDK-bootstrapped** in the target region
  (`cdk bootstrap aws://<account>/<region>`) if it has not been already.

## The Lambda binaries build themselves

Every Lambda in the app is a Go binary on the `provided.al2023` custom runtime
targeting **arm64/Graviton**. There is no separate build step: the stack uses
`GoFunction` from `@aws-cdk/aws-lambda-go-alpha`, which compiles each entrypoint
during `cdk synth` and names the output `bootstrap` as the custom runtime
requires.

```sh
cd infrastructure
pnpm install
```

The equivalent of what it runs per function, for `<fn>` in `authorization`,
`admin` and `copycompletion`:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o bootstrap ./functions/<fn>
```

`GOOS`, `GOARCH`, and `CGO_ENABLED=0` come from the construct (`GOARCH` follows
the function's `architecture`); `-trimpath -ldflags "-s -w"` is set via
`bundling.goBuildFlags` in `lib/data-releaser-stack.ts`. `moduleDir` points at
the repo-root `go.mod`, since the Go module spans the whole repository rather
than `infrastructure/`. It builds with the local Go toolchain when one is on
`PATH`, and falls back to a Docker container otherwise — so a machine with Go
installed needs no Docker, and a machine with neither Go nor Docker cannot
synthesise.

Because the asset hash is derived from the compiled binary, a Go source change
is picked up automatically on the next synth. There is no build step to forget
and no stale binary to deploy.

## Synthesise and deploy

From the `infrastructure/` directory:

```sh
pnpm run build     # tsc typecheck/compile (optional but recommended)
pnpm test          # CDK assertion tests over the synthesised template

pnpm exec cdk synth                     # synthesise (no credentials needed)
pnpm exec cdk deploy DataReleaserStack  # deploy to the target account/region
```

Name the stack you mean. The app holds more than one as soon as a dataset is
configured, so a bare `cdk deploy` no longer resolves to a single target:
`cdk deploy DataReleaserStack`, `cdk deploy HotCache-<name>`, or
`cdk deploy 'HotCache-*'`.

Neither command takes any `-c` flags: all configuration is read from
`infrastructure/cdk.context.json` (see below). `cdk synth` needs no credentials
as long as `account`/`region` there match the VPC lookup cached in that same
file. A lookup for a new account, region or `apiVpcId` needs
credentials once to populate that cache. `cdk deploy` always uses your resolved
AWS credentials.

Two further stacks exist for demos only and are off by default —
`DataShareTestStack` (a disposable bucket seeded with the htsget-rs sample BAM)
and `MockMartStack` (an Athena stand-in for the OrcaVault Mart). Neither belongs
in a real deployment; see the
[`infrastructure/README.md`](../infrastructure/README.md) for both.

## Targeting an account and region

Everything that varies between environments is a **parameter**, never hardcoded
— so one source tree deploys to a development and a production Data Control
account with no code change. Every parameter lives under the `dataReleaser` key
of `infrastructure/cdk.context.json`; edit that file to retarget a deployment.

Start from the example: `cp infrastructure/cdk.context.example.json
infrastructure/cdk.context.json`. The real file is gitignored, because it holds
your account IDs, VPC and the subnets CDK looks up. Keep it with the rest of
your deployment configuration, for example in a private repository, and copy or
link it into `infrastructure/` before running CDK.

```jsonc
// infrastructure/cdk.context.json
{
  "dataReleaser": {
    "account": "999988887777",
    "region": "ap-southeast-2",
    "apiVpcId": "vpc-0345678901bcdef02",
    "researcherAccountIds": ["111111111111", "222222222222"]
    // ... see the tables below
  },
  // The VPC lookup cache CDK maintains itself — leave it alone.
  "vpc-provider:account=999988887777:filter.vpc-id=...": { }
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
> one to refresh a stale VPC lookup, restore the block afterwards from your
> saved copy of the file, or delete just the `vpc-provider:` entry by hand
> instead.

### Context parameters

| Context key | Meaning | Default |
| --- | --- | --- |
| `account` | Target AWS account ID | `CDK_DEFAULT_ACCOUNT`, else a placeholder |
| `region` | Target region | `CDK_DEFAULT_REGION`, else `ap-southeast-2` |
| `namePrefix` | Prefix applied to physical resource names | `data-releaser` |
| `stageName` | API Gateway REST stage name (also emitted as an output) | `prod` |
| `researcherAccountIds` | Accounts granted manifest read; JSON array of account-ID strings, may be empty | `[]` |
| `cacheTtlCeiling` | Ceiling on the authorization response cache lifetime, Go duration string | `1h` |
| `cacheDenyTtl` | Negative-cache max-age, Go duration string | `60s` |
| `provisionOperatorPolicy` | Provision the optional managed policy an operator runs the CLI with | `false` |
| `apiVpcId` | VPC hosting the API's interface endpoint and all Lambdas; must be the VPC the htsget-rs Lambda runs in. **Required** — there is no public deployment mode | `vpc-0345678901bcdef02` |
| `copyStateMachineArn` | State machine performing dataset copies; setting it deploys the copy completion Lambda and the EventBridge rule feeding it | unset — neither is deployed |
| `copyWorkingBucketName` | Bucket the copy orchestration works in, holding its summary of each run. **Required alongside `copyStateMachineArn`** | — |

Onboarding a researcher's manifest read access is a configuration change: add
their account ID to `dataReleaser.researcherAccountIds` in `cdk.context.json`
and redeploy. Each account gets one bucket-policy statement granting
`s3:GetObject` on `manifests/<account-id>/*` and `datasets/<account-id>/*` — its
own prefix under each mechanism, and nothing else. Both prefixes are granted
whichever mechanism the account currently holds, because which one it holds
changes without this stack being redeployed, and a manifest written to an
ungranted prefix fails silently: the write is same-account and succeeds, and the
document is descriptive only, so the researcher keeps working access and simply
cannot discover where the data is.

> **The stack provisions no authorization cache.** Authorization responses are
> cached solely by the htsget-rs callout client, governed by the `Cache-Control`
> and `Vary` directives the authorization Lambda emits — which is what
> `cacheTtlCeiling` and `cacheDenyTtl` configure. There is no API Gateway
> response cache, so there is no provisioned cache cluster to pay for and nothing
> to flush.

### Per-dataset stacks

A dataset released as direct S3 access gets a bucket of its own and a KMS key
used by nothing else, in a `HotCache-<name>` stack of its own — so scheduling
deletion of one dataset's key destroys that dataset's copy and can reach no
other. Each entry in `datasets` produces one such stack, deployed by name.

```jsonc
// infrastructure/cdk.context.json
{
  "dataReleaser": {
    "copyWriterRoleName": "steps-s3-copy-role",
    "datasets": [
      { "name": "cohort-a", "datasetUri": "s3://source-bucket/datasets/cohort-a/" }
    ]
  }
}
```

| Context key | Meaning | Default |
| --- | --- | --- |
| `datasets` | Datasets to deploy, as `{ name, datasetUri, bucketName?, dataRetentionDays? }` objects | `[]` |
| `copyWriterRoleName` | Role the copy orchestration writes through; each dataset key policy grants it. **Required once a dataset is configured** | — |
| `copyWriterRoleAccount` | Account owning that role, when the copy orchestration runs elsewhere | this account |
| `custodianRoleArn` | Principal permitted to disable a dataset key and schedule its deletion | key administrators only |
| `obscureBucketNames` | Derive bucket names with a keyed hash instead of a legible slug | `false` |
| `dataRetentionDays` | Storage-cost backstop, in days; must be at least 90 | `365` |
| `keyPendingWindowDays` | Days a dataset key stays recoverable after deletion is scheduled | `7` |
| `logObjectAccess` | Record object reads via S3 server access logging | `true` |
| `accessLogsBucketName` | Existing bucket to receive those logs | one per dataset |

`name` becomes the stack name, so it must start with a letter and hold only
letters, digits and hyphens, and it must not change once deployed. Synth rejects
a duplicated `name` and a duplicated `datasetUri`, since two entries sharing a
URI derive one bucket name and contend for it.

Bucket names deserve care, because the buckets and keys are retained on delete:
a rename does not move a copy, it abandons it and stands an empty bucket up
beside it. `obscureBucketNames` requires the hash key in the
`HOT_CACHE_BUCKET_NAME_HASH_KEY` environment variable and synth fails without
it rather than falling back to a legible name — an environment variable rather
than context because `cdk.context.json` is normally kept in version control, and
a key kept there obscures nothing. The CLI derives the same name from the same inputs, so its
`s3.hotCache` settings and this variable have to agree with the deployment's; see
[`docs/configuration.md`](configuration.md). Once a bucket holds a copy someone
is reading, pin its name with the dataset's `bucketName` and the derivation
stops mattering.

Retention here is a **cost backstop, not an expiry mechanism**: the lifecycle
rule deletes objects and rounds to the following midnight UTC, so access granted
again after a deletion would need a fresh multi-hour copy. Expiry of a
researcher's access is a time condition in the bucket and key policies instead,
which is why the stack refuses a retention shorter than 90 days.

The CLI never deploys these stacks. It checks that a dataset's bucket and key
exist and names the deployment that owns them when they do not — otherwise it
would need a Node and CDK toolchain plus deployment-grade credentials on the very
path that grants a researcher access, and standing up a bucket and a key would
become a side effect of a release command.

## Stack outputs

`DataReleaserStack` exports five CloudFormation outputs. Three of them map
directly onto the CLI configuration fields that reference deployed resources
(see [`docs/configuration.md`](configuration.md)), so those values are read from
the deployment rather than guessed:

| Output | CLI config field | Notes |
| --- | --- | --- |
| `ApiGatewayRestApiId` | `endpoints.apiGatewayRestApiId` | REST API ID; consumed by the `validate` API Gateway stage check. |
| `ApiGatewayStageName` | `endpoints.apiGatewayStageName` | Stage name (equals `stageName`; default `prod`). |
| `ManifestBucketName` | `manifest.bucket` | Manifest bucket name (`<namePrefix>-manifests-<account>`). |
| `AuthorizationCalloutUrl` | `endpoints.authorization` | The callout URL (`<api-url>authorize`). This is also the URL htsget-rs's `[auth.authorization]` callout config points at; it sends a bare `GET` carrying the requester in the `Htsget-Context-SourceVpcArn` header, and a `?sourceVpcArn=...` query parameter is still accepted for probing it by hand. Private DNS keeps the standard `execute-api` hostname — it simply does not resolve outside the VPC. |
| `AuthorizationApiVpcEndpointId` | — | The `execute-api` interface endpoint that is the sole route to the API; the resource policy denies everything with a different `aws:SourceVpce`. |

Because the API is private, `endpoints.authorization` is not reachable
from an operator's machine. Set `validation.skipAuthorizationCheck` so `validate`
reports that probe as `SKIP` instead of failing on a healthy deployment; see
[`infrastructure/README.md`](../infrastructure/README.md#api-authorization) for
the reasoning. To check the endpoint itself, call it from a host inside the VPC.

Read the outputs after deploying, for example:

```sh
aws cloudformation describe-stacks \
  --stack-name DataReleaserStack \
  --query "Stacks[0].Outputs" --output table
```

If `provisionOperatorPolicy=true`, the stack additionally exports
`OperatorPolicyArn` — the ARN of the optional operator IAM policy, which carries
Athena and Glue read, the table and its indexes, the API stage lookup, invoke on
the admin routes, the Lattice and RAM reads `validate` makes, and write access to
the manifest bucket. Provisioning it does not change the rule that the CLI
creates no IAM identity or managed policy at runtime; it is deployment-time
infrastructure attached to operators out of band.

## After deployment: wire up and verify

1. Copy the three mapped outputs into your CLI config
   (`endpoints.apiGatewayRestApiId`, `endpoints.apiGatewayStageName`,
   `manifest.bucket`) — see [`docs/configuration.md`](configuration.md).
2. Run `data-releaser validate` to confirm the Data Control resources are
   present and consistent with configuration (the DynamoDB table, the manifest
   bucket, and the API Gateway stage), alongside the other provider-side checks.
   Naming no mechanism checks both; `validate s3` or `validate htsget` narrows
   the run to one. See
   [`docs/usage.md`](usage.md#validate--validate-the-sharing-infrastructure).

## Redeploying and teardown

Redeploy with the same command after changing parameters or the Lambda Go
sources; synth recompiles the binaries. The DynamoDB table and the manifest
bucket use a `RETAIN` removal policy, so they are **not** destroyed when the
stack is deleted — expired and revoked records and researcher manifests are
audit data and must survive stack lifecycle operations. Each dataset's cache
bucket and key are retained for the same reason, with the added one that a
destroyed key destroys the copy it encrypts and there is no way back short of a
fresh multi-hour transfer. Removing any of them requires a deliberate, separate
action.
