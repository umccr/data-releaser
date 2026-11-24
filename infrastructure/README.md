# @umccr/data-releaser-cdk

AWS CDK stacks that deploy, into the Data Control account, every AWS resource the
[data releaser](https://github.com/umccr/data-releaser)'s two release mechanisms
need. Use them from a CDK app of your own; each version of this package is
released together with the `data-releaser` CLI of the same version.

| Resource                         | Notes                                                                                                                                                                                                                                            |
|----------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| DynamoDB `ShareRecords`          | PK `shareId`; GSI1 `targetVpcArn` (callout lookup); GSI2 `researcherAccountId`+`createdAt` (list/idempotency); GSI3 `datasetUri`+`createdAt` (releases of one dataset); **no TTL**                                                                                                                        |
| Authorization Lambda             | Go binary from `functions/authorization`; reads GSI1 only                                                                                                                                                                                        |
| Admin Lambda                     | Go binary from `functions/admin`; read/write table + GSI2                                                                                                                                                                                        |
| Copy completion Lambda + rule    | Go binary from `functions/copycompletion`, deployed only when `copyStateMachineArn` is set; an EventBridge rule on that state machine's execution status changes moves a staging dataset to staged or failed — see [copy completion](#copy-completion) |
| API Gateway **REST** API + stage | no gateway cache and no required request parameters — htsget-rs sends a bare `GET /authorize` with the requester identity in the `Htsget-Context-SourceVpcArn` header, and caches the response itself using the `Cache-Control` the Lambda emits |
| API reachability                 | `PRIVATE` endpoint + `execute-api` interface endpoint in `apiVpcId`, with a resource policy pinned to it — see [API authorization](#api-authorization)                                                                                           |
| API method authorization         | `/admin/*` additionally requires **SigV4** (`AuthorizationType.IAM`); `GET /authorize` is unauthenticated by necessity and relies on the private endpoint                                                                                        |
| Lambda execution roles           | scoped to the table and the single index each function queries                                                                                                                                                                                   |
| Manifest bucket + bucket policy  | private, block-public-access; per-account `s3:GetObject` on `manifests/<account-id>/*`                                                                                                                                                           |
| Operator IAM policy              | optional (`provisionOperatorPolicy`)                                                                                                                                                                                                             |

TypeScript (not CDK Go) because the surrounding UMCCR deployments — including the
htsget VPC Lattice constructs this system depends on — are CDK TypeScript.

The stack provisions **nothing** outside the Data Control account: no
researcher-side resources, no Lake Formation / RAM sharing, no htsget-rs.

Each dataset released as direct S3 access gets a **hot cache stack** of its own —
see [per-dataset hot cache stacks](#per-dataset-hot-cache-stacks). Those are
separate stacks, deployed by name; `DataReleaserStack` does not depend on them and
they do not depend on it.

## Usage

```bash
npm install @umccr/data-releaser-cdk aws-cdk-lib constructs
```

```ts
import * as cdk from "aws-cdk-lib";
import { DataReleaserStack, hotCacheStacks } from "@umccr/data-releaser-cdk";

const app = new cdk.App();
const env = { account: "999988887777", region: "ap-southeast-2" };

new DataReleaserStack(app, "DataReleaserStack", {
  env,
  apiVpcId: "vpc-0345678901bcdef02",
  researcherAccountIds: ["111111111111", "222222222222"],
});

hotCacheStacks(app, {
  env,
  copyWriterRoleName: "steps-s3-copy-writer-role",
  datasets: [{ name: "cohort-a", datasetUri: "s3://source-bucket/datasets/cohort-a/" }],
});
```

```bash
npx cdk deploy DataReleaserStack
```

Pin this package to the version of the `data-releaser` CLI you run against the
deployment. Deploying needs Node.js or Bun and the CDK CLI, but no Go toolchain or
Docker: the Lambda functions ship prebuilt. The
[deployment guide](https://github.com/umccr/data-releaser/blob/main/docs/deployment.md)
walks through a complete deployment app.

| `DataReleaserStack` prop  | Meaning                                                                                                               | Default                                     |
|---------------------------|-----------------------------------------------------------------------------------------------------------------------|---------------------------------------------|
| `env`                     | Target account and region. **Required** in practice, because the VPC lookup needs both                                | —                                           |
| `apiVpcId`                | VPC hosting the API's interface endpoint and every Lambda; must be the VPC the htsget-rs Lambda runs in. **Required** | —                                           |
| `namePrefix`              | Physical-name prefix                                                                                                  | `data-releaser`                             |
| `stageName`               | REST API stage name                                                                                                   | `prod`                                      |
| `researcherAccountIds`    | Accounts granted manifest read                                                                                         | `[]`                                        |
| `cacheTtlCeiling`         | Cache TTL ceiling (Go duration)                                                                                       | `1h`                                        |
| `cacheDenyTtl`            | Negative-cache max-age (Go duration)                                                                                  | `60s`                                       |
| `provisionOperatorPolicy` | Provision the managed policy an operator assumes to run the CLI                                                       | `false`                                     |
| `copyStateMachineArn`     | State machine performing dataset copies; set it to deploy the copy completion Lambda and its rule                      | unset — neither is deployed                 |
| `copyWorkingBucketName`   | Bucket the copy orchestration works in, holding its summary of each run. **Required alongside `copyStateMachineArn`**   | —                                           |

## Lambda binaries

Every Lambda is a **Go** binary on the `provided.al2023` custom runtime, targeting
**arm64/Graviton**, built from `functions/<name>` in the repository. The package
ships them as `dist/assets/<name>/bootstrap`, the name the custom runtime requires.

In a checkout, `bun run build:lambdas` builds them into `assets/` (Go required).
Per function it runs:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false -tags lambda.norpc -ldflags "-s -w" \
  -o assets/<fn>/bootstrap ./functions/<fn>
```

The binaries carry no VCS stamp, so with the same Go version an unchanged function
builds to an identical asset and is not redeployed.

## Copy completion

Copying a dataset into its bucket runs for hours or days, so nothing waits on it.
The copy orchestration announces its own terminal state, an EventBridge rule
delivers that to `functions/copycompletion`, and the release the copy was staging
becomes staged — grantable — or failed, which grants nothing and refuses to be
released.

```ts
new DataReleaserStack(app, "DataReleaserStack", {
  // ...
  copyStateMachineArn: "arn:aws:states:ap-southeast-2:999988887777:stateMachine:steps-s3-copy",
  copyWorkingBucketName: "steps-s3-copy-working",
});
```

Set neither and neither resource is deployed, which is what a deployment releasing
only through htsget wants. Set one without the other and synth fails: without the
working bucket a finished copy is recorded with none of the figures describing it,
and without the state machine nothing listens at all and a copied dataset sits in
the staging state forever.

The rule matches **every** status change on that state machine, terminal or not.
Which statuses end a copy is the function's to know, and enumerating them in the
rule as well would split that decision in two — with the half in the rule failing
by dropping an event silently. A running execution costs one invocation that
records nothing, and a cost estimate shares the state machine but is named after a
dataset rather than a release, so it matches no record and is reported as staging
nothing.

The execution role gets `GetItem` and `UpdateItem` on the table and nothing else
there — a copy is named after the release it stages, so the record is addressed by
key rather than searched — plus `states:DescribeExecution` on that state machine's
executions, for the events whose payload the bus dropped for being too large, and
`s3:GetObject` on the working bucket, for the copy's summary of what it moved.

It holds **no** access to any dataset bucket or key, deliberately. The copy writes
that same summary into the dataset's own bucket, where it is encrypted with that
dataset's key; reading it there would mean this role holding decrypt on every
dataset key that will ever exist, extended by hand with each new dataset, on the
one component whose whole job is to write a few numbers into a table. The working
bucket's copy also outlives a purge of the dataset, which is the other reason it is
the one read.

A copy orchestration installed in another account needs a rule there forwarding its
execution events onto this account's bus; the rule here matches only what reaches
it.

## Per-dataset hot cache stacks

A dataset released as plain S3 access lives in a bucket of its own, encrypted with
a KMS key used by nothing else. One `HotCache-<name>` stack owns that pair, so
scheduling deletion of one dataset's key destroys that dataset's copy and reaches
no other. `HotCacheStack` is the stack, and `hotCacheStacks` turns the configured
list of datasets into instances of it.

Grants are **not** deployed here. The CLI writes the researcher's access into the
bucket and key policies when a release is made and rewrites both documents whole
on every change, so this stack establishes only the baseline they are built on.
The copy orchestration, the source object store, and everything in a researcher
account are consumed, not deployed: none of them appear in these stacks.

Each dataset is one entry in the `datasets` array:

```ts
hotCacheStacks(app, {
  env,
  copyWriterRoleName: "steps-s3-copy-writer-role",
  custodianRoleArn: "arn:aws:iam::999988887777:role/dataset-custodian",
  datasets: [{ name: "cohort-a", datasetUri: "urn:fdc:example.org:2025:dataset/cohort-a" }],
});
```

```bash
cdk deploy HotCache-cohort-a
```

| `hotCacheStacks` option | Meaning                                                                                   | Default             |
|-------------------------|-------------------------------------------------------------------------------------------|---------------------|
| `env`                   | Account and region every dataset stack is deployed to                                      | —                   |
| `datasets`              | Datasets to deploy, as `{ name, datasetUri, bucketName?, dataRetentionDays? }` objects     | —                   |
| `namePrefix`            | Physical-name prefix; must match `DataReleaserStack` and the CLI                           | `data-releaser`     |
| `copyWriterRoleName`    | Role the copy orchestration writes through; each dataset key policy grants it. **Required once a dataset is configured** | —  |
| `copyWriterRoleAccount` | Account owning that role, when the copy orchestration runs elsewhere                       | this account        |
| `custodianRoleArn`      | Principal allowed to disable a dataset key and schedule its deletion                       | key admins only     |
| `bucketNameHashKey`     | Secret that derives bucket names with a keyed hash instead of a legible slug                | unset — legible     |
| `dataRetentionDays`     | Storage-cost backstop, in days; must be ≥ 90 and well beyond any release expiry            | `365`               |
| `keyPendingWindowDays`  | Days a dataset key stays recoverable after deletion is scheduled                            | `7`                 |
| `logObjectAccess`       | Record object reads via S3 server access logging                                            | `true`              |
| `accessLogsBucketName`  | Existing bucket to receive those logs; must already permit log delivery                     | one per dataset     |

`name` is an operator-chosen handle: it becomes the stack name, so it must start
with a letter and hold only letters, digits and hyphens, and it must not change
once deployed. It stays legible even when bucket names are obscured, because stack
names are visible only inside this account and never to the party the data goes
to. Synth rejects a duplicated name and a duplicated `datasetUri` — two entries
sharing a URI would derive one bucket name and contend for it.

Two things about names are worth care, because both end in a **new empty bucket
beside a full one that nothing refers to** — the buckets and keys are retained on
delete, so a rename orphans rather than destroys:

- To obscure names, pass `bucketNameHashKey: resolveBucketNameHashKey(true, process.env)`.
  It reads the key from the `HOT_CACHE_BUCKET_NAME_HASH_KEY` environment variable
  and fails without it rather than falling back to a legible name. The variable,
  not the app, because a deployment app is normally kept in version control and a
  key kept there obscures nothing. The same value must be present on every synth.
- Starting or stopping obscuring, changing the hash key, or editing a `datasetUri`
  all rename the bucket. Once a bucket holds a copy someone is reading, pin its
  name with the dataset's `bucketName` and the derivation stops mattering.

The retention rule is a **cost backstop, not an expiry mechanism**: it deletes
objects, and lifecycle rounds to the following midnight UTC, so a release
re-granted after a deletion would need a fresh multi-hour copy. Access expiry is a
time condition in the bucket and key policies instead, which is why the stack
refuses a retention shorter than 90 days.

### Deployed out of band, not by the CLI

An operator deploys a dataset's stack before staging data into it. The CLI checks
that the bucket and key exist and reports the deployment that owns them when they
do not; it never creates them. Otherwise the CLI would need a Node and CDK
toolchain plus deployment-grade credentials on the same path that grants a
researcher access, and standing up a bucket and a key would become a side effect
of a release command.

One consequence for the other stacks: with datasets configured an app holds more
than one stack, so a bare `cdk deploy` no longer resolves. Name what you mean —
`cdk deploy DataReleaserStack`, `cdk deploy HotCache-<name>`, or
`cdk deploy 'HotCache-*'`.

## Development app and demo stacks

This repository's own CDK app, `bin/data-releaser.ts`, is for developing the
stacks. It reads its configuration from the `dataReleaser` key of a gitignored
`cdk.context.json` (start from `cdk.context.example.json`), and it also holds two
demo-only stacks that are not part of the package. Real deployments use the
package from an app of their own, as above.

### Optional: mock test-data bucket (`DataShareTestStack`)

In the real system the shareable reads live in a Data Account bucket outside this
repo. For the mock/demo set-up there is no such account, so an **optional** stack
stands one up: a locked-down, disposable bucket named
`data-share-test-<account>-<region>`, seeded with the public htsget-rs sample BAM
that htsget-rs serves and the mock Mart advertises. Eventually it is the fixture
an end-to-end integration test resolves against.

It is **off by default** and deploys only when `dataReleaser.dataShareTest` is
`true` in `cdk.context.json`. The seed data is vendored, so there is no
preparatory step:

```bash
cdk deploy DataShareTestStack
```

`test-data/` holds the upstream `htsnexus_test_NA12878.bam` and its `.bai` index
(2.5 MB, MIT, from `umccr/htsget-rs`), renamed and laid out in the bucket's key
structure (`demo/L2400001/NA12878.bam`); `cdk deploy` uploads that tree verbatim. See
[`test-data/README.md`](test-data/README.md) for provenance and checksums. Synth
still fails fast if a `TEST_DATA_OBJECTS` key has no file behind it, rather than
deploying a bucket that 404s in htsget.

The bucket blocks all public access, enforces TLS, disables ACLs
(`BUCKET_OWNER_ENFORCED`), and is destroyed with the stack (it holds only
reproducible public sample data). Set `s3://data-share-test-<account>-<region>`
as the htsget-rs static location; the account-qualified name also scopes the
derived `s3:GetObject` grant to this account.

| Context key     | Meaning                              | Default |
|-----------------|--------------------------------------|---------|
| `dataShareTest` | Deploy the mock test-data bucket     | `false` |

### Optional: mock OrcaVault Mart (`MockMartStack`)

A second, **optional** stack stands in for the OrcaVault Mart so the CLI can be
demoed or exercised end-to-end without warehouse access. It is not part of the
deployment proper and is **off by default**; it deploys only when
`dataReleaser.mockMart` is `true` in `cdk.context.json`, and only if you also
name the stack:

```bash
cdk deploy MockMartStack
```

It creates a disposable S3 bucket, a Glue database with a `bam` table whose
columns are the ones `internal/warehouse` selects (backed by JSON-lines rows the
stack writes), an Athena workgroup, and a managed policy granting operators the
S3 access Athena needs on that bucket.

The workgroup sets **and enforces** `resultConfiguration.outputLocation`. This is
required, not cosmetic: the CLI calls `StartQueryExecution` with no
`ResultConfiguration` of its own, so a workgroup without an output location fails
every query with *"No output location provided"*.

`mockMartDatabase` and `mockMartWorkgroup` must match `warehouse.database` and
`warehouse.athenaWorkgroup` in the CLI config. They default to mock-specific names
(`mart_mock` / `data-releaser-mock`) rather than the real `mart` / workgroup, so
the mock can be deployed **alongside** the real warehouse's Lake Formation `mart`
resource link in the same account without a Glue-catalog name collision. (One
caveat in a strictly Lake-Formation-governed catalog: the mock uses plain IAM, so
the mock database/table may also need an LF `SELECT`/`DESCRIBE` grant, or to be
left in IAM/hybrid access mode, to be queryable.) Point the CLI at the mock by
setting `warehouse.database`/`warehouse.athenaWorkgroup` to these names.

| Context key          | Meaning                                                      | Default                              |
|----------------------|--------------------------------------------------------------|--------------------------------------|
| `mockMart`           | Deploy the mock Mart at all                                  | `false`                              |
| `mockMartDatabase`   | Glue database name → `warehouse.database`                    | `mart_mock`                          |
| `mockMartWorkgroup`  | Athena workgroup → `warehouse.athenaWorkgroup`               | `data-releaser-mock`                 |
| `mockMartDataBucket` | Bucket named in the rows' `bucket` column (not created here) | `data-share-test-<account>-<region>` |
| `mockMartCohortId`   | Value of the `cohort_id` column                              | `DEMO`                               |
| `mockMartObjects`    | Rows as `{ libraryId, key }` objects (JSON array)             | the `TEST_DATA_OBJECTS` fixtures     |

The `mockMartDataBucket` default is the `DataShareTestStack` bucket, so the two
stacks are meant to be deployed together: the rows then resolve to objects that
actually exist. Override it to point the rows at a different bucket.

## API authorization

Two layers, because the two path trees have different constraints.

**Layer 1 — the network.** The REST API is a `PRIVATE` endpoint. The only route
in is the `execute-api` interface endpoint this stack creates in `apiVpcId`, and
the API resource policy pairs an allow conditioned on `aws:SourceVpce` with an
explicit deny for everything else. Outside that VPC the hostname does not
resolve, and a request that somehow arrived anyway would be denied. This is what
protects `GET /authorize`, which cannot be authenticated: htsget-rs issues the
callout as a plain unsigned `GET`, and the callout config has no way to attach a
credential (its `forward.headers` is an allow/deny filter over the *client's*
headers, not a way to inject one).

Private DNS on the endpoint means the URL is unchanged —
`https://<rest-api-id>.execute-api.<region>.amazonaws.com/<stage>/authorize`
still, so `AuthorizationCalloutUrl` and htsget-rs's `HTSGET_AUTH_AUTHORIZATION`
need no adjustment. It just only resolves inside the VPC.

**Layer 2 — IAM, on the admin routes only.**

| Route | Auth | Why |
| --- | --- | --- |
| `GET /authorize` | network only | Unsigned by necessity. Only reachable from the VPC the htsget-rs Lambda runs in. |
| `POST /admin/shares`, `GET /admin/shares`, `POST /admin/shares/revoke`, `POST /admin/shares/extend` | **SigV4** (`AWS_IAM`) + network | These create, revoke and extend shares, so they additionally require a signed request. |

Callers of `/admin/*` need `execute-api:Invoke` on
`arn:aws:execute-api:<region>:<account>:<rest-api-id>/<stage>/*/admin/*` **and**
have to be inside the VPC. The optional operator policy
(`provisionOperatorPolicy`) carries the permission as its `InvokeAdminApi`
statement. From an instance in the VPC:

```bash
awscurl --service execute-api --region ap-southeast-2 \
  "https://<rest-api-id>.execute-api.ap-southeast-2.amazonaws.com/prod/admin/shares"
```

A signed request from a principal without the permission returns
`403 {"Message":"User ... is not authorized to perform: execute-api:Invoke..."}`;
an unsigned one returns `403 {"message":"Missing Authentication Token"}`; one
from outside the VPC does not connect at all.

The CLI does not use these routes — `share`, `list`, `revoke` and `extend` talk
to DynamoDB directly — so none of this affects it. It does affect `validate`,
whose Authorization Service check now has to be turned off from outside the VPC
with `validation.skipAuthorizationCheck`.

### Prerequisites — no escape hatch

`apiVpcId` must be a VPC with **private subnets that have egress** — the same VPC
the htsget-rs Lambda is attached to in its own deployment. The stack
looks that VPC up rather than creating it, and touches nothing in it beyond the
interface endpoint and security groups (the endpoint's allows 443 from the VPC
CIDR; the Lambdas' allows 443 egress only). It does
**not** add an S3 gateway endpoint: the VPC needs its own route to S3 (NAT or a
gateway endpoint) or htsget-rs will authorize requests it cannot then serve.

There is deliberately **no public escape hatch**: the former `privateApi=false`
context flag has been removed, and nothing in its place makes any of this
optional. Both Lambdas are always attached to `apiVpcId`'s private-with-egress
subnets, the REST API is always `PRIVATE` behind a resource policy pinned to the
interface endpoint, and every bucket blocks all public access. Making anything
public again requires editing the stack — a reviewable code change, not a
context value.

The VPC lookup is resolved against the real account **once** and cached in the
deployment app's `cdk.context.json`, so later synths work with no credentials.
Commit that file with the app. Only a lookup for a new account/region/VPC needs
credentials to populate the cache.

## Stack outputs → CLI config

| Output | CLI config field |
| --- | --- |
| `ApiGatewayRestApiId` | `endpoints.apiGatewayRestApiId` |
| `ApiGatewayStageName` | `endpoints.apiGatewayStageName` |
| `ManifestBucketName` | `manifest.bucket` |
| `AuthorizationCalloutUrl` | `endpoints.authorization` |
| `AuthorizationApiVpcEndpointId` | — (the only route to the API) |

## Development

```bash
bun install             # install dependencies (respects the lockfile)
bun run test            # build the Lambda binaries, then run the CDK assertion tests
bun run build           # build the package into dist/: Lambda binaries, JS, type declarations
bunx cdk synth          # synthesise the development app from cdk.context.json
```

Releases are automated: release-please tags `vX.Y.Z` on `main`, and the release
workflow publishes this package to npm and the CLI to the GitHub release from the
same tag.
