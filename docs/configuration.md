# Configuration

The `data-releaser` CLI is configured with a YAML file. The file contains no
secrets — only a credential-source identifier and endpoint/behaviour settings.
The Data Control credentials themselves are provisioned externally and resolved
through the standard AWS credential chain (see
[`docs/usage.md`](usage.md#authentication)).

The file covers both release mechanisms. Some sections are shared, the rest
belong to one mechanism or the other, and a command only reads — and only
validates — the sections it actually uses.

## Config file search paths

When `--config <path>` is **not** supplied, the tool searches these locations in
order and uses the first one that exists:

1. `./config.yaml` (the current working directory)
2. `~/.config/data-releaser/config.yaml`

If `--config <path>` **is** supplied, that exact file is used; if it does not
exist, the command fails immediately. If no config file can be resolved at all,
the command fails with a message listing the locations it looked in.

## Flag precedence

CLI flags override the corresponding config-file values. Currently one config
field is overridable by a global flag:

| Config field | Overriding flag | Effect |
| --- | --- | --- |
| `profile` | `--profile <name>` | The flag wins when set (non-empty). If the flag is omitted, the config-file `profile` is used; if that is also empty, the default AWS credential chain is used. |

Per-command inputs such as `--libraries`, `--dataset-uri`, `--account`,
`--target-vpc-id`, `--expiry` and `--duration` are command arguments, not config
fields — they have no config-file equivalent. Neither does the mechanism: it is
named as part of the invocation, and there is deliberately no way to default it
in a file, because a default that is wrong for one command grants access by the
wrong means without anything failing at the time.

## How validation works

Loading the file parses it and applies defaults. It checks nothing else.

Each command then declares the sections it reads and only those are checked,
before any AWS call is made. This matters because the file covers two release
mechanisms that share almost no fields: an `s3` command must be able to run
without `warehouse` or `endpoints.htsgetBaseUrl` configured, and an `htsget`
command without the `s3` section. A command reports every problem in the
sections it declared at once, so one run names everything there is to fix.

The consequence worth knowing: a field being wrong is only reported by commands
that read it. `validate` is the broadest check available, and running it after a
config change is the cheapest way to find a mistake in a section your last
command happened not to touch.

Which sections each command reads:

| Command | Sections read |
| --- | --- |
| `share htsget` | `warehouse`, `ledger`, `manifest`, `endpoints.htsgetBaseUrl`, `researchers` |
| `extend htsget` | `ledger`, `manifest`, `endpoints.htsgetBaseUrl` |
| `revoke htsget` | `ledger` |
| `stage s3` | `account`, `ledger`, `s3.copy`, `s3.source`, `s3.hotCache` |
| `share s3` | `account`, `ledger`, `s3.copy.writerRoleName`, `s3.custodianRoleArn`, `s3.manifest` |
| `extend s3` | as `share s3` |
| `revoke s3` | as `share s3` |
| `purge s3` | as `share s3` |
| `restore s3` | `ledger` |
| `delete s3` | `ledger` |
| `list` | `ledger` |
| `validate htsget` | `warehouse`, `ledger`, `manifest`, `endpoints.authorization`, `endpoints.htsgetBaseUrl`, `researchers` |
| `validate s3` | `account`, `ledger`, `s3.copy`, `s3.source`, `s3.copy.writerRoleName`, `s3.custodianRoleArn` |
| `validate` | both of the above |

`revoke htsget` deliberately reads only the ledger. Stopping access must not be
blocked by configuration belonging to the granting path, so the manifest is
republished afterwards on a best-effort basis and a missing manifest target is a
warning rather than a refusal.

`revoke s3` reads the same sections as `share s3`, which looks like the opposite
choice and is not. The two documents it writes are rendered whole rather than
edited, so taking the granting statements out means producing both documents
again — and the baseline they fall back to still has to name the role the next
copy writes through and the principal permitted to withdraw the data. A
withdrawal that could not render those would strand the dataset it was meant to
protect.

`restore s3` and `delete s3` read only the ledger, which is less than `purge s3`
and not an oversight in either case. Neither renders a policy or publishes a
document — no grant changes, so there is nothing new for either to say — and the
key one recovers and the bucket the other empties are the ones the copy records
name rather than ones derived from the dataset URI. The fields a rendering would
need are fields they never touch.

`validate` invoked bare covers both mechanisms, so it reads both sets. A
mechanism that is not configured at all is reported as a failed check rather than
refusing the run, and the other mechanism's checks still run; naming a mechanism
makes its configuration a hard requirement, since there is then nothing else to
report on.

## Fields

Legend: **Required** fields must be present and non-empty, or the commands that
read that section fail before doing anything. **Optional** fields may be
omitted; a default is applied where noted.

## Shared sections

### Top level

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `profile` | optional | *(default AWS chain)* | Name of an AWS profile/credential source the SDK understands (SSO, `credential_process`, etc.). Omit to use the default credential chain. Overridable with `--profile`. |
| `account` | required by `s3` commands | — | The AWS account owning the ledger table, the manifest bucket, and every dataset bucket and key. It is written into policy documents as a principal, so it must be the 12-digit account ID and not an alias. |
| `region` | required by `s3` commands | — | The region those resources live in. A dataset key policy confines the researcher's decrypt permission to S3 in this one region, so a wrong value denies every read instead of failing visibly. |

### `ledger` — the shared record store

Both mechanisms write their records to one table, discriminated by mechanism, so
that "what does this account have access to?" has a single answer.

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `ledger.table` | optional | `data-releaser-ShareRecords` | The DynamoDB table holding the records. The deployment creates it as `<namePrefix>-ShareRecords`, so take the name from that stack's output rather than relying on the default. |

### `manifest` — the manifest bucket

The bucket is shared by both mechanisms; each has its own key template, so a
researcher holding access through both gets two documents rather than one
overwriting the other. The dataset document's template lives at
`s3.manifest.keyTemplate`.

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `manifest.bucket` | **required** | — | The manifest bucket name, sourced from a stack output. Never hard-code it. Required by `share htsget`, `extend htsget`, and `validate htsget`; `revoke htsget` reports a missing value as a warning rather than refusing to revoke. |
| `manifest.keyTemplate` | optional | `manifests/{accountId}/manifest.json` | Per-account object key template for the htsget document. Only the `{accountId}` placeholder is supported; any other `{...}` token is rejected. |

Each researcher's manifest is written at the rendered key, e.g.
`manifests/777788889999/manifest.json`.

## htsget mechanism sections

### `warehouse` — OrcaVault Mart via Athena (Lake Formation share)

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `warehouse.athenaWorkgroup` | **required** | — | Athena workgroup in the Data Control (consumer) account. Results land in this workgroup's configured S3 output location. |
| `warehouse.database` | **required** | — | The Lake Formation resource-link database name (e.g. `mart`). |
| `warehouse.catalog` | optional | `AwsDataCatalog` | The Glue catalog (resource-link) to query. |

### `endpoints` — service endpoints

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `endpoints.authorization` | **required** | — | Base URL of the Authorization Service (used by `validate` reachability checks). |
| `endpoints.htsgetBaseUrl` | **required** | — | htsget-rs base URL embedded in generated manifests so researchers can build requests. |
| `endpoints.apiGatewayRestApiId` | optional | — | REST API ID of the authorization service's API Gateway. Sourced from a stack output. Consumed by the `validate` API Gateway stage check, which reports the check as unavailable if it is unset. |
| `endpoints.apiGatewayStageName` | optional | `prod` | API Gateway stage name. Sourced from a stack output. Used together with the REST API ID by the `validate` API Gateway stage check. |

### `cache` — authorization response caching

These durations are Go duration strings (`1h`, `90m`, `60s`).

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `cache.ttlCeiling` | optional | `1h` | The `s-maxage` ceiling for allow responses (caps how long a decision may be reused; also the upper bound on revocation propagation). |
| `cache.denyTtl` | optional | `60s` | Negative-cache TTL for deny responses, so newly created shares become visible promptly. |

> These fields configure the CLI's view of the cache policy. The authoritative
> values used at request time come from the `Cache-Control` directives the
> deployed Authorization Service Lambda emits, capped by the TTL ceiling
> configured on the htsget-rs callout cache (see
> [`docs/deployment.md`](deployment.md)).

### `validation.canary` — optional canary dataset

Used only by `validate` (the Cross-Account S3 check and `--test-setup` mode).
The whole `validation.canary` block is optional; when absent, the canary-based
checks report themselves as skipped/unavailable.

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `validation.canary.libraryId` | optional | — | Library ID of a known canary dataset for the synthetic test share. |
| `validation.canary.account` | optional | — | Researcher account used for the synthetic canary share. |
| `validation.canary.bucket` | optional | — | Data Account bucket holding the canary object (for the cross-account HeadObject check). |
| `validation.canary.key` | optional | — | Object key of the canary object. |

### `validation` — skipping unreachable checks

Two `validate` checks make HTTP calls to endpoints that, in the VPC-isolated
topology this system deploys, deliberately do not resolve from an operator's
machine. Neither flag weakens anything — they turn a guaranteed false failure
into an explicit `SKIP`, and both endpoints are verified from inside the VPC
instead.

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `validation.skipHtsgetCheck` | optional | `false` | Skip the htsget-rs `/service-info` health check. Set it when htsget-rs is fronted by VPC Lattice: its domain resolves only from a VPC associated with the service network. |
| `validation.skipAuthorizationCheck` | optional | `false` | Skip the Authorization Service reachability probe. Set it when the authorization API is deployed private (the default) — it is served by an `execute-api` interface endpoint in the data account VPC, and its resource policy denies anything arriving another way. |

### `researchers` — per-account htsget identity

One entry per researcher account, keyed by account ID.

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `researchers.<accountId>.targetVpcArn` | **required** for each entry | — | ARN of the researcher VPC associated with the htsget VPC Lattice service network, e.g. `arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-0123456789abcdef0`. This is the value the authorization callout is keyed on and it must be the *EC2 VPC* ARN VPC Lattice puts on the invocation event — not a `vpc-lattice` ARN. The lookup is an exact match, so any other value silently denies every request. Overridable per invocation with `--target-vpc-arn`. |

Every entry is checked whenever a command declares the `researchers` section, not
just the account being shared with, because a malformed ARN produces a share that
can never authorize anything and the failure surfaces only as an unexplained
deny.

The `s3` mechanism does not read this section. It takes a VPC in a different
form — the bare `vpc-…` identifier, compared literally against the VPC a request
to S3 arrived from — and takes it per grant rather than from configuration, so
one researcher may hold two grants on one copy from two different VPCs.

## s3 mechanism sections

Read only by the `s3` commands. The copy orchestration and the source store are
deployed and operated elsewhere; this section is how they are located.

### `s3.copy` — the copy orchestration

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `s3.copy.stateMachineArn` | **required** | — | ARN of the state machine a copy is started on. The orchestration is deployed separately, so an installation in another account is a config change rather than a code change. |
| `s3.copy.workingBucket` | **required** | — | Bucket the orchestration reads its copy instructions from and writes its own outputs to. It belongs to that orchestration's installation, not to this one. |
| `s3.copy.workingBucketPrefix` | optional | *(bucket root)* | Prefix within the working bucket the orchestration was deployed with. Every key computed for that bucket sits beneath it. |
| `s3.copy.writerRoleName` | **required** | — | Name of the IAM role the orchestration writes objects through. Each dataset key policy has to name it, or a copy is denied the data key it needs partway through a transfer that may already have run for hours. |
| `s3.copy.account` | optional | *(the top-level `account`)* | Account the orchestration is installed in. Set it when the orchestration does not live alongside the dataset buckets. |
| `s3.copy.writerRoleAccount` | optional | *(the top-level `account`)* | Account owning `writerRoleName`, when it differs. |
| `s3.copy.performance.headConcurrency` | optional | `0` | How many source objects are examined at once, before any data moves. Zero leaves the orchestration's own default in place. |
| `s3.copy.performance.smallCopyConcurrency` | optional | `0` | How many small-object copy workers run at once. Zero leaves the orchestration's default. |
| `s3.copy.performance.smallCopyBatchSize` | optional | `0` | How many small objects each worker is given. A batch is copied one object at a time, so this multiplies into how long a worker runs against a fixed timeout. Zero leaves the orchestration's default. |
| `s3.copy.performance.largeCopyConcurrency` | optional | `0` | How many large objects transfer at once. The heaviest sustained read the source sees. Zero leaves the orchestration's default. |

The performance settings travel with each copy rather than being fixed when the copy
orchestration is deployed, because the right values follow the source store and one
installation may copy from several. The defaults suit a source that scales with the load
put on it. A source on a fixed link is saturated by them, and the resulting failure is
reported against the small-object copies rather than the large transfers that caused it:
their requests queue behind the large ones until a worker exceeds its timeout, and because
that stage tolerates no failures, one late batch fails the entire run.

### `s3.source` — the external object store

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `s3.source.endpointUrl` | **required** | — | The store's custom S3 endpoint (`http` or `https`). Only the shape is checked here; whether it is routable from the subnets the copy orchestration runs in is a property of the deployment. |
| `s3.source.region` | optional | — | The region the store presents itself as being in. S3-compatible stores generally require some value even where the concept does not apply. |
| `s3.source.credentialsSecret` | **required** | — | Secrets Manager secret holding the store's access key. Only the identifier appears here and only the identifier travels: the copy orchestration resolves the secret with its own permissions, so no source credential passes through the CLI. |

### `s3.hotCache` — dataset bucket naming

A dataset's bucket and key are deployed out of band, one stack per dataset. The
CLI only locates them, so it has to arrive at the same name the deployment did,
from the same inputs.

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `s3.hotCache.namePrefix` | optional | `data-releaser` | Prefix applied to derived bucket names. It must match the prefix the deployment used: a different prefix derives a different name, and therefore a different bucket. |
| `s3.hotCache.obscureBucketNames` | optional | `false` | Derive names with a keyed hash rather than from a legible slug of the dataset URI. When set, the key must be supplied in the `HOT_CACHE_BUCKET_NAME_HASH_KEY` environment variable, and commands refuse to run without it — falling back to the legible form would silently point at a different bucket. Must match the deployment's `obscureBucketNames` context value and the same environment variable at synth time. |
| `s3.hotCache.buckets` | optional | — | Map of dataset URI to an explicit bucket name, bypassing derivation. Worth setting once a bucket holds a copy someone is reading, since derivation depends on inputs that may change. Two URIs may not map to one bucket: purging a dataset destroys the key its bucket is encrypted with. |

The hash key is deliberately not a config field. The config file lives beside the
deployment's checked-in context, and a checked-in key obscures nothing — anyone
holding it can reproduce a bucket name from a guessed dataset URI.

### `s3.manifest` and `s3.custodianRoleArn`

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `s3.manifest.bucket` | optional | *(the shared `manifest.bucket`)* | Where the dataset access document is delivered. Defaults to the shared manifest bucket, so a deployment naming one bucket does not name it twice. |
| `s3.manifest.keyTemplate` | optional | `datasets/{accountId}/manifest.json` | Per-account key template for the dataset document. A separate prefix from the htsget template so the two documents can share a bucket. Only `{accountId}` is supported. |
| `s3.custodianRoleArn` | optional | — | ARN of the principal permitted to withdraw a dataset by disabling its key or scheduling the key's deletion. Omitted, the statement is not written and that power stays with the account's key administrators. Must match the deployment's `custodianRoleArn`. |

## Annotated example

```yaml
# config.yaml — data-releaser configuration (no secrets live here)

# Single Data Control identity. Any profile the AWS SDK understands (SSO,
# credential_process, ...). Omit to use the default credential chain.
# Overridable at runtime with --profile.
profile: "data-control"

# Shared: the account and region every resource lives in. Required by the s3
# commands, which write the account into policy documents as a principal and
# condition a researcher's decrypt permission on the region.
account: "999988887777"
region:  "ap-southeast-2"

ledger:                                 # shared: the record store both mechanisms write to
  table: "data-releaser-ShareRecords"   # optional; "<namePrefix>-ShareRecords" from the deployment

manifest:                                              # shared bucket, per-mechanism key prefix
  bucket:      "data-releaser-manifests-999988887777"  # from the stack output; never hard-code
  keyTemplate: "manifests/{accountId}/manifest.json"   # optional; the htsget document

# --- htsget mechanism ------------------------------------------------------

warehouse:                          # OrcaVault Mart via Athena (Lake Formation share)
  athenaWorkgroup: "primary"        # REQUIRED — workgroup in the Data Control (consumer) account
  database:        "mart"           # REQUIRED — Lake Formation resource-link database
  catalog:         "AwsDataCatalog" # optional — Glue catalog; defaults to AwsDataCatalog

endpoints:
  authorization:   "https://auth.example.org"           # REQUIRED
  htsgetBaseUrl:   "https://htsget-lattice.example.org"  # REQUIRED
  apiGatewayRestApiId: "abc123def4"  # from the stack output; used by the validate stage check # pragma: allowlist secret
  apiGatewayStageName: "prod"        # from the stack output; defaults to "prod"

cache:
  ttlCeiling: "1h"    # optional — s-maxage ceiling for allow responses (default 1h)
  denyTtl:    "60s"   # optional — negative cache for deny responses (default 60s)

validation:
  skipHtsgetCheck: true         # htsget-rs only resolves inside a VPC on the lattice network
  skipAuthorizationCheck: true  # the authorization API is private to the data account VPC
  canary:                       # optional — used only by `validate`
    libraryId: "L2400999"
    account:   "777788889999"
    bucket:    "example-data-bucket"
    key:       "canary/known-object.bam"

researchers:                      # the EC2 VPC ARN the authorization callout is keyed on
  "777788889999":
    targetVpcArn: "arn:aws:ec2:ap-southeast-2:777788889999:vpc/vpc-0123456789abcdef0"

# --- direct-read s3 mechanism ---------------------------------------------

s3:
  copy:                           # the copy orchestration, deployed separately
    stateMachineArn: "arn:aws:states:ap-southeast-2:999988887777:stateMachine:steps-s3-copy"  # REQUIRED
    workingBucket:   "steps-s3-copy-working-999988887777"  # REQUIRED — belongs to that installation
    workingBucketPrefix: ""       # optional — the prefix the orchestration was deployed with
    writerRoleName:  "steps-s3-copy-writer"  # REQUIRED — each dataset key policy names it
    # account:           "999988887777"  # optional — set if installed in another account
    # writerRoleAccount: "999988887777"  # optional — set if that role lives elsewhere
  source:                         # the external S3-compatible store
    endpointUrl:       "https://ceph.example.org"  # REQUIRED
    region:            "us-east-1"                 # optional — some value is usually required
    credentialsSecret: "ceph/data-releaser"        # REQUIRED — the identifier only # pragma: allowlist secret
  hotCache:
    namePrefix:         "data-releaser"  # optional — must match the deployment's prefix
    obscureBucketNames: false            # optional — needs HOT_CACHE_BUCKET_NAME_HASH_KEY when true
    # buckets:                           # optional — pin a dataset to an explicit bucket
    #   "s3://ceph-bucket/cohort-a/": "data-releaser-dataset-0123456789abcdef"
  manifest:
    keyTemplate: "datasets/{accountId}/manifest.json"  # optional; bucket defaults to manifest.bucket
  custodianRoleArn: "arn:aws:iam::999988887777:role/DatasetCustodian"  # optional — the kill switch
```

## Mapping to stack outputs

Three config values are meant to be read from the deployed CDK stack outputs
rather than guessed. See [`docs/deployment.md`](deployment.md#stack-outputs) for
the full mapping:

| Config field | Stack output |
| --- | --- |
| `endpoints.apiGatewayRestApiId` | `ApiGatewayRestApiId` |
| `endpoints.apiGatewayStageName` | `ApiGatewayStageName` |
| `manifest.bucket` | `ManifestBucketName` |
