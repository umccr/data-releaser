# Deployment

The Data Control account resources this project owns are deployed with the AWS
CDK stacks published to npm as
[`@umccr/data-releaser-cdk`](https://www.npmjs.com/package/@umccr/data-releaser-cdk)
(source under `infrastructure/`). There are two kinds of stack:

- **`DataReleaserStack`** — the `ShareRecords` DynamoDB table, the authorization
  and admin Lambdas, the copy completion Lambda when a copy orchestration is
  configured, the API Gateway REST API, their execution roles, and the manifest
  bucket with its cross-account read policy.
- **`HotCache-<name>`** — one stack per dataset released as direct S3 access,
  holding that dataset's cache bucket and the KMS key encrypting it, and nothing
  else. Each is deployed by name, on its own, and neither it nor
  `DataReleaserStack` depends on the other.

The stacks deploy **nothing outside the Data Control account** — no
researcher-side resources, no Lake Formation / RAM sharing, not htsget-rs, and
not the copy orchestration that moves a dataset into a cache bucket. Those are
owned elsewhere; [`docs/prerequisites.md`](prerequisites.md) lists what each
account has to have in place and who owns it.

The [`infrastructure/README.md`](../infrastructure/README.md) is the package
reference; this document is the operator-facing walkthrough.

## A deployment is a CDK app of your own

The configuration of a deployment (account, VPC, researcher accounts, datasets)
is not stored in this repository. Keep it in a small CDK app of your own,
usually in a private repository next to the CLI's `config.yaml`:

```
my-data-releaser-deployment/
  package.json        # @umccr/data-releaser-cdk, aws-cdk-lib, constructs, aws-cdk
  cdk.json            # "app": "npx tsx bin/app.ts" (or "bun bin/app.ts")
  bin/app.ts          # the stacks and their props
  cdk.context.json    # CDK's own lookup cache, committed
  config.yaml         # the CLI configuration for the same account
```

```ts
// bin/app.ts
import * as cdk from "aws-cdk-lib";
import { DataReleaserStack, HotCacheStacks } from "@umccr/data-releaser-cdk";

const app = new cdk.App();
const env = { account: "999988887777", region: "ap-southeast-2" };

new DataReleaserStack(app, "DataReleaserStack", {
  env,
  apiVpcId: "vpc-0345678901bcdef02",
  researcherAccountIds: ["111111111111", "222222222222"],
});

HotCacheStacks.forDatasets(app, {
  env,
  copyWriterRoleName: "steps-s3-copy-role",
  datasets: [{ name: "cohort-a", datasetUri: "s3://source-bucket/datasets/cohort-a/" }],
});
```

The package supports `aws-cdk-lib` 2.178.0 or later and `constructs` 10, as peer
dependencies, so your app chooses its own CDK version within those ranges. Pin
`@umccr/data-releaser-cdk` to the same version as the `data-releaser` CLI you run
against it: both are released together from one tag, and they share
the ledger schema, the manifest prefixes and the bucket naming. Use the feature
flags from [`infrastructure/cdk.json`](../infrastructure/cdk.json) in your
`cdk.json`, which are the ones the stacks are tested with.

Keep the stack IDs (`DataReleaserStack`, and the `HotCache-<name>` IDs
`HotCacheStacks.forDatasets` derives) stable once deployed. A different ID is a different
CloudFormation stack, which would try to create a second copy of every named
resource beside the first.

## Toolchain and credentials

- **Node.js** (24 or later) or **Bun**, and the AWS CDK CLI (`aws-cdk`) as a dev
  dependency of your app. No Go toolchain or Docker is needed: the package ships
  the Lambda functions prebuilt.
- **AWS credentials** for the target Data Control account, resolvable through
  the standard chain (a profile, SSO, environment variables, or an assumed
  role) with permission to create the stacks' resources.
- The account must be **CDK-bootstrapped** in the target region
  (`cdk bootstrap aws://<account>/<region>`) if it has not been already.

## Synthesise and deploy

From your deployment app:

```sh
npx cdk synth                      # synthesise
npx cdk diff DataReleaserStack     # review the change against what is deployed
npx cdk deploy DataReleaserStack   # deploy to the target account/region
```

Name the stack you mean. The app holds more than one as soon as a dataset is
configured, so a bare `cdk deploy` no longer resolves to a single target:
`cdk deploy DataReleaserStack`, `cdk deploy HotCache-<name>`, or
`cdk deploy 'HotCache-*'`.

`DataReleaserStack` looks up `apiVpcId`, so it needs an explicit `env` with a
concrete account and region, and the first synth for a new account, region or
VPC needs credentials. CDK records the result in your app's `cdk.context.json`;
commit it, and later synths work without credentials. `cdk deploy` always uses
your resolved AWS credentials.

## Targeting an account and region

Everything that varies between environments is a **prop**, never hardcoded, so
one package version deploys to a development and a production Data Control
account. `env` is the standard CDK `StackProps` field.

### `DataReleaserStack` props

| Prop | Meaning | Default |
| --- | --- | --- |
| `env` | Target account and region. **Required** in practice, because the VPC lookup needs both | — |
| `apiVpcId` | VPC hosting the API's interface endpoint and all Lambdas; must be the VPC the htsget-rs Lambda runs in. **Required** — there is no public deployment mode | — |
| `namePrefix` | Prefix applied to physical resource names | `data-releaser` |
| `stageName` | API Gateway REST stage name (also emitted as an output) | `prod` |
| `researcherAccountIds` | Accounts granted manifest read, as account-ID strings; may be empty | `[]` |
| `cacheTtlCeiling` | Ceiling on the authorization response cache lifetime, Go duration string | `1h` |
| `cacheDenyTtl` | Negative-cache max-age, Go duration string | `60s` |
| `provisionOperatorPolicy` | Provision the optional managed policy an operator runs the CLI with | `false` |
| `copyStateMachineArn` | State machine performing dataset copies; setting it deploys the copy completion Lambda and the EventBridge rule feeding it | unset — neither is deployed |
| `copyWorkingBucketName` | Bucket the copy orchestration works in, holding its summary of each run. **Required alongside `copyStateMachineArn`** | — |

Onboarding a researcher's manifest read access is a configuration change: add
their account ID to `researcherAccountIds` and redeploy. Each account gets one
bucket-policy statement granting
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
other. `HotCacheStacks.forDatasets` turns each entry in `datasets` into one such stack,
deployed by name.

```ts
HotCacheStacks.forDatasets(app, {
  env,
  copyWriterRoleName: "steps-s3-copy-role",
  datasets: [{ name: "cohort-a", datasetUri: "s3://source-bucket/datasets/cohort-a/" }],
});
```

| Option | Meaning | Default |
| --- | --- | --- |
| `env` | Account and region every dataset stack is deployed to | — |
| `datasets` | Datasets to deploy, as `{ name, datasetUri, bucketName?, dataRetentionDays? }` objects | — |
| `namePrefix` | Prefix applied to physical resource names; must match `DataReleaserStack` and the CLI | `data-releaser` |
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
beside it. `obscureBucketNames` reads the hash key from the
`HOT_CACHE_BUCKET_NAME_HASH_KEY` environment variable and fails without it,
rather than falling back to a legible name. The key is never configuration,
because a deployment app is normally kept in version control and a key kept
there obscures nothing. The CLI derives the same name from the same inputs, so its
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

The development app in `infrastructure/bin/` additionally holds two demo-only
stacks, `DataShareTestStack` and `MockMartStack`, which are not part of the
package; see the [`infrastructure/README.md`](../infrastructure/README.md).

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
