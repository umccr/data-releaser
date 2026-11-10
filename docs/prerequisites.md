# Prerequisites

Data Releaser operates across several AWS accounts, but it only *deploys* the
resources in one of them (the Data Control account, via `infrastructure/`).
Everything else it depends on is deployed and owned by another project. This page
is the per-account prerequisite checklist. For each item it states the owning
project and how to verify the item is in place.

The single most useful verification tool is the `validate` command
([`docs/usage.md`](usage.md#validate--validate-the-sharing-infrastructure)).
Where a check below can be exercised by `validate`, that is noted. A failed check
names the resource that is missing, the account holding it and the party who
supplies it, rather than passing on whichever AWS call happened to notice —
"bucket not found" does not tell an operator whether to deploy a stack, chase
another team, or fix a config field.

> **Legend for "Prerequisite for"**
> - **Infra** — must exist (or is deployed by this repo) for the CDK stack to deploy.
> - **CLI** — must exist for the CLI to run a command successfully.
> - **Researcher** — must exist for a researcher account to consume shared data.
>
> Items marked **(htsget)** or **(s3)** belong to one release mechanism only;
> unmarked items are needed by both. A deployment that uses one mechanism does not
> need the other's prerequisites, and will not be told otherwise: configuration is
> validated per command, so a missing htsget field never blocks an s3 command.

---

## Data Control Account (916218008751)

This account hosts the authorization service, htsget-rs, VPC Lattice, the
OrcaVault Lake Formation resource link, the manifest bucket, and one cache bucket
and key per dataset released as direct S3 access.

### Deployed by this repo (`infrastructure/` CDK app)

These are created when you `cdk deploy` the stacks in `infrastructure/` (see
[`docs/deployment.md`](deployment.md)). They are prerequisites for the CLI, not
for the stack itself.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| Authorization callout and admin Lambdas (htsget) | This repo | CLI | `aws lambda get-function --function-name <name>`. `validate`'s reachability check only works from inside the API's VPC; from anywhere else set `validation.skipAuthorizationCheck` and invoke the callout Lambda directly instead (`aws lambda invoke` with a `{"headers":{"Htsget-Context-SourceVpcArn":"..."}}` payload). |
| Copy completion Lambda and its EventBridge rule (s3) | This repo, only when `copyStateMachineArn` and `copyWorkingBucketName` are configured | CLI | `aws lambda get-function --function-name <namePrefix>-copy-completion` and `aws events describe-rule --name <namePrefix>-copy-completion`. Without them a copy's outcome is never recorded: the copy stays in the copying state forever and nothing can be granted against it. |
| DynamoDB `ShareRecords` table + GSI1 (`targetVpcArn`) + GSI2 (`researcherAccountId`/`createdAt`) + GSI3 (`datasetUri`/`createdAt`), **no TTL** | This repo | CLI | `aws dynamodb describe-table --table-name <name>`; confirm all three GSIs exist and `TimeToLiveDescription` is disabled. Both mechanisms record into this one table, and `validate` reads it in each of its two halves. |
| API Gateway REST API and stage (htsget) | This repo | CLI | `aws apigateway get-stage --rest-api-id <id> --stage-name <stage>`. `validate` checks the configured stage exists — that is a control-plane call, so it works from anywhere even though the API itself is private. |
| `execute-api` interface endpoint + API resource policy pinned to it (htsget) | This repo (always; the stack cannot be deployed public) | Researcher | `aws apigateway get-rest-api --rest-api-id <id>` shows `endpointConfiguration.types = ["PRIVATE"]` and the endpoint ID; `get-rest-api --query policy` shows the allow/deny pair on `aws:SourceVpce`. This is the only control on `GET /authorize`, which cannot be authenticated. |
| Lambda execution roles for the above | This repo | CLI | `aws iam get-role --role-name <name>`; scoped to the table and the indexes each function queries. |
| Manifest bucket + cross-account read bucket policy | This repo | CLI, Researcher | `aws s3api head-bucket --bucket <name>`; `aws s3api get-bucket-policy --bucket <name>` and confirm each onboarded account has `s3:GetObject` on its own `manifests/<account-id>/*` and `datasets/<account-id>/*` prefixes. `validate` checks the bucket is present and writable. |
| One cache bucket and one KMS key per dataset, in a `HotCache-<name>` stack each (s3) | This repo, deployed by name and out of band | CLI, Researcher | `aws s3api head-bucket --bucket <name>` and `aws kms describe-key --key-id alias/<name>` — the key is found by an alias derived from the bucket name. `stage s3` checks both before it starts a transfer, reports them together (an operator who has not deployed a dataset's stack is missing both), and refuses rather than creating either. A key that is not enabled is refused too: the copy asks for a data key per object, so a disabled key fails the transfer partway through, after hours. |
| Operator/admin IAM policy | This repo (optional) | CLI | Optional. If provisioned, `aws iam get-policy` / attach to the operator principal. The CLI creates no IAM identity or managed policy at runtime — the access it grants is written into bucket and key policies instead. |

### Deployed elsewhere (assumed prerequisites)

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| htsget-rs Lambda and its `[auth.authorization]` callout config pointing at the authorization API (htsget) | elsewhere | CLI, Researcher | `validate` performs an htsget-rs health/service-info request. Manually: hit the htsget-rs service-info endpoint. |
| A VPC (`UomPrimaryVpc`, `vpc-03e01fb6290012661`) with private subnets that have egress **and a route to S3** (NAT or an S3 gateway endpoint) | elsewhere | Infra, Researcher | `aws ec2 describe-subnets --filters Name=vpc-id,Values=<id>` and `aws ec2 describe-vpc-endpoints --filters Name=vpc-id,Values=<id>`. Neither this repo's stack nor htsget-pipeline creates the VPC or the S3 route; the stack looks it up and the htsget Lambda joins it (`vpcOrName`) so it can reach the private authorization API, and without an S3 route htsget-rs authorizes requests it then cannot serve. |
| VPC Lattice service, service network, and RAM share exposing the htsget-rs endpoint (htsget) | elsewhere | Researcher | `aws vpc-lattice list-services` / `list-service-networks`; `aws ram get-resource-shares --resource-owner SELF`. Per-account association is checked by `validate --account <id>`. |
| Lake Formation `mart` resource-link database, `SELECT`/`DESCRIBE` grants, and Athena workgroup + S3 results location (htsget) | elsewhere | CLI | `validate` runs a probe query (`LIMIT 1` against `mart.bam`) through the Lake Formation share. Manually: `aws athena start-query-execution` against the workgroup. |
| htsget-rs cross-account read role (used for `s3:GetObject`/`HeadObject` on the Data Account) (htsget) | elsewhere | Researcher | `validate` performs a cross-account `HeadObject` on a known canary object. |
| Copy service state machine (s3) | the separately deployed copy service | CLI | `aws stepfunctions describe-state-machine --state-machine-arn <arn>`. `validate` re-describes the most recent copy this deployment started on it, which confirms the ARN in configuration is the one those copies ran on. Before the first copy it can only describe an execution that cannot exist, which establishes the account, region and permission but not that the state machine is deployed — the same answer comes back either way, and the check says so. Nothing copies data into this account without it. |
| Copy working bucket, where that service reads its instructions and writes its report of each run (s3) | the separately deployed copy service | CLI | `validate` writes a probe object beneath the configured prefix, since a copy that cannot be instructed fails hours after it was asked for. The copy completion Lambda reads that same report, which is why `copyWorkingBucketName` is configured on both sides. |
| The role the copy service writes objects through (s3) | the separately deployed copy service | CLI | `aws iam get-role --role-name <name>`. Its name is fixed by that service's deployment and has to be given identically to the CDK app (`copyWriterRoleName`) and the CLI (`s3.copy.writerRoleName`): each dataset's key policy names it, and a mismatch fails a transfer partway through after it may already have run for hours. |

---

## Warehouse Account (115253169271) — htsget mechanism

Hosts OrcaHouse/OrcaVault. Data Releaser never authenticates into this account; it
consumes the Mart through the Lake Formation share in the Data Control account.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| OrcaVault and its curated `mart` layer, including `mart.bam` | OrcaHouse | CLI | Indirectly via `validate`'s probe query against `mart.bam`. A schema-conformant `LIMIT 1` result confirms the Mart is reachable and shaped as expected. |
| The Lake Formation share exposing the Mart to the Data Control account | OrcaHouse | CLI | Same as above — a successful probe query through the resource-link database confirms the share is accepted and granted. |

> Resolution reads the most recent daily OrcaVault build and may lag live OrcaBus state
> by up to roughly a day. See `docs/operations.md`.

---

## Data Account (503977275616) — htsget mechanism

Holds the bioinformatics files that htsget-rs serves in place, without copying
them anywhere.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| S3 bucket holding the bioinformatics files | elsewhere | Researcher | Indirectly via `validate`'s canary `HeadObject`. |
| Bucket policy granting `s3:GetObject`/`s3:HeadObject` to the Data Control role htsget-rs reads through | elsewhere | Researcher | `validate`'s cross-account `HeadObject` check confirms the policy is effective. Manually: `aws s3api get-bucket-policy --bucket <data-bucket>`. |

---

## Source object store — s3 mechanism

The external, S3-compatible store holding the authoritative copy of every dataset
the s3 mechanism releases. It is not an AWS account and nothing here is ours to
deploy; the copy service reads it, and this project only names it.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| The store's S3 endpoint (`s3.source.endpointUrl`), routable from the subnets the copy service runs in | the store's operator | CLI | `validate` checks only that the endpoint is a well-formed URL and reports the check as `SKIP`: it is reached from the copy service's subnets, not from an operator's machine, so a lookup made here would say nothing either way. A real answer comes from a `stage s3 --dry-run`. |
| A Secrets Manager secret in the Data Control account holding that store's access key (`s3.source.credentialsSecret`) | the store's operator supplies the credentials; this account holds the secret | CLI | `aws secretsmanager describe-secret --secret-id <id>`; the copy service needs read access to it. Only the identifier travels — the copy service resolves the secret with its own permissions, so no source credential passes through the CLI. |

---

## Researcher Account (per onboarded account)

Deployed and owned by that account's onboarding deployment. A cross-account
manifest read requires *both* the resource-side grant (on the manifest bucket
policy, owned by Data Control) and this identity-side grant. See
`docs/onboarding.md` for the full procedure.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| Accepted AWS RAM resource share and active VPC Lattice service network association (htsget) | Researcher onboarding | Researcher | `validate --account <id>` checks the association is active and the RAM share is accepted. |
| IAM identity-based policy letting the account's own principals read its manifest from the Data Control manifest bucket | Researcher onboarding | Researcher | The resource-side grant is checked by `validate --account <id>`; the identity-side policy is verified in the researcher account (`aws iam simulate-principal-policy` or an actual `s3:GetObject` from that account against `manifests/<account-id>/manifest.json`, or `datasets/<account-id>/manifest.json` for the s3 mechanism). |
| The nominated VPC reads arrive from, an S3 endpoint in it, and read permissions on the account's own principals (s3) | Researcher onboarding | Researcher | The bucket policy compares the bare VPC identifier passed to `share s3 --target-vpc-id` literally against the VPC a request arrives from, so a read from anywhere else is denied. None of this side is provisioned here: a denial on it is reported and attributed to the researcher account rather than fixed. |
| Test probe Lambda (optional in-VPC probe) (htsget) | htsget VPC Lattice consumer construct | Researcher (optional) | `validate --account <id>` discovers any function in the account whose name contains `TestLambda` and invokes it; if none exists it reports the in-VPC probe as unavailable and recommends the consumer construct's `deployTestLambda` option. |

---

## Operator credentials

Data Control account credentials must be available through the standard AWS
credential chain (an SSO/`credential_process` profile, environment variables, or
an assumed role); see `docs/configuration.md` for how the CLI selects the
credential source. `validate` confirms they resolve, via
`sts:GetCallerIdentity` — that they resolve is all a single call can establish,
so the individual checks that follow are what actually exercise the permissions.

The optional operator policy the stack can provision (`provisionOperatorPolicy`)
is the enumeration of what the htsget path needs: Athena, Glue and Lake Formation
read for library resolution, read and write on the ledger table and its indexes,
the API stage lookup, invoke on the admin routes, the Lattice and RAM reads
`validate` makes, and write access to the manifest bucket.

It does not cover the s3 verbs, which additionally read and write the dataset
bucket and key policies, write objects into a cache bucket and the copy working
bucket, start and describe executions on the copy state machine, schedule a
dataset key's deletion on `purge s3`, cancel that deletion and re-enable the key
on `restore s3`, and delete the copied objects on `delete s3`. An operator
running those verbs needs those permissions from elsewhere. Where a custodian is
configured (`s3.custodianRoleArn`), the key policy admits it for
`kms:DisableKey`, `kms:ScheduleKeyDeletion` and `kms:DescribeKey`, so withdrawing
a dataset is reachable by a principal other than the one that granted it; with no
custodian configured the statement is omitted and withdrawal stays with the
account's own key administrators.
