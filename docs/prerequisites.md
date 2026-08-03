# Prerequisites

The Data Releaser operates across several AWS accounts, but it only *deploys* the
resources in one of them (the Data Control account, via `infrastructure/`). Everything
else it depends on is deployed and owned by another project. This page is the
per-account prerequisite checklist derived from the deployment ownership table in
`design.md`. For each item it states the owning project and how to verify the item is
in place.

The single most useful verification tool is the `validate` command
(`docs/operations.md`, Requirement 15). Where a check below can be exercised by
`validate`, that is noted; `validate` names the owning account rather than surfacing a
bare AWS error (Req 17.7).

> **Legend for "Prerequisite for"**
> - **Infra** — must exist (or is deployed by this repo) for the Infrastructure_Stack to deploy.
> - **CLI** — must exist for the Data_Releaser CLI to run a command successfully.
> - **Researcher** — must exist for a Researcher_Account to consume shared data.

---

## Data Control Account (916218008751)

This account hosts the Authorization_Service, htsget-rs, VPC Lattice, the OrcaVault
Lake Formation resource link, and the Manifest_Bucket.

### Deployed by this repo (`infrastructure/` CDK app)

These are created when you `cdk deploy` the Infrastructure_Stack (see
`docs/deployment.md`). They are prerequisites for the CLI, not for the stack itself.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| Authorization_Service Lambdas (callout + admin) | This repo | CLI | `aws lambda get-function --function-name <name>`. `validate`'s reachability check only works from inside the API's VPC; from anywhere else set `validation.skipAuthorizationCheck` and invoke the callout Lambda directly instead (`aws lambda invoke` with a `{"headers":{"Htsget-Context-SourceVpcArn":"..."}}` payload). |
| DynamoDB `ShareRecords` table + GSI1 (`targetVpcArn`) + GSI2 (`researcherAccountId`/`createdAt`), **no TTL** | This repo | CLI | `aws dynamodb describe-table --table-name <name>`; confirm both GSIs exist and `TimeToLiveDescription` is disabled. `validate` reports the table under its Infrastructure_Stack checks (Req 15.10). |
| API Gateway REST API and stage | This repo | CLI | `aws apigateway get-stage --rest-api-id <id> --stage-name <stage>`. `validate` checks the configured stage exists (Req 15.10) — that is a control-plane call, so it works from anywhere even though the API itself is private. |
| `execute-api` interface endpoint + API resource policy pinned to it | This repo (always; the stack cannot be deployed public) | Htsget_Rs | `aws apigateway get-rest-api --rest-api-id <id>` shows `endpointConfiguration.types = ["PRIVATE"]` and the endpoint ID; `get-rest-api --query policy` shows the allow/deny pair on `aws:SourceVpce`. This is the only control on `GET /authorize`, which cannot be authenticated. |
| Lambda execution roles for the above | This repo | CLI | `aws iam get-role --role-name <name>`; scoped to the table and the indexes each function queries. |
| Manifest_Bucket + cross-account read bucket policy | This repo | CLI, Researcher | `aws s3api head-bucket --bucket <name>`; `aws s3api get-bucket-policy --bucket <name>` and confirm each onboarded account has `s3:GetObject` on its own `manifests/<account-id>/*` prefix. `validate` checks the bucket is present and writable (Req 15.10). |
| Operator/admin IAM policy (Req 11.2) | This repo (optional) | CLI | Optional. If provisioned, `aws iam get-policy` / attach to the operator principal. The CLI never creates IAM at runtime (Req 11.3). |

### Deployed elsewhere (assumed prerequisites)

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| Htsget_Rs Lambda and its `[auth.authorization]` callout config pointing at the Authorization_Service | elsewhere | CLI, Researcher | `validate` performs an htsget-rs health/service-info request (Req 15.2d). Manually: hit the htsget-rs service-info endpoint. |
| A VPC (`UomPrimaryVpc`, `vpc-03e01fb6290012661`) with private subnets that have egress **and a route to S3** (NAT or an S3 gateway endpoint) | elsewhere | Htsget_Rs | `aws ec2 describe-subnets --filters Name=vpc-id,Values=<id>` and `aws ec2 describe-vpc-endpoints --filters Name=vpc-id,Values=<id>`. Neither this repo's stack nor htsget-pipeline creates the VPC or the S3 route; the htsget Lambda joins it (`vpcOrName`) so it can reach the private authorization API, and without an S3 route it authorizes requests it then cannot serve. |
| VPC Lattice service, service network, and RAM share exposing the Htsget_Rs endpoint | elsewhere | Researcher | `aws vpc-lattice list-services` / `list-service-networks`; `aws ram get-resource-shares --resource-owner SELF`. Per-account association is checked by `validate --account <id>` (Req 15.3). |
| Lake Formation `mart` resource-link database, `SELECT`/`DESCRIBE` grants, and Athena workgroup + S3 results location | elsewhere | CLI | `validate` runs a probe query (`LIMIT 1` against `mart.bam`) through the Lake Formation share (Req 15.2b). Manually: `aws athena start-query-execution` against the workgroup. |
| Htsget_Rs cross-account read role (used for `s3:GetObject`/`HeadObject` on the Data Account) | elsewhere | Researcher | `validate` performs a cross-account `HeadObject` on a known canary object (Req 15.2e). |

---

## Warehouse Account (115253169271)

Hosts OrcaHouse/OrcaVault. The Data_Releaser never authenticates into this account; it
consumes the Mart through the Lake Formation share in the Data Control account.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| OrcaVault and its curated `mart` layer, including `mart.bam` | OrcaHouse | CLI | Indirectly via `validate`'s probe query against `mart.bam` (Req 15.2b). A schema-conformant `LIMIT 1` result confirms the Mart is reachable and shaped as expected. |
| LakeFormation_Share exposing the Mart to the Data Control account | OrcaHouse | CLI | Same as above — a successful probe query through the resource-link database confirms the share is accepted and granted. |

> Resolution reads the most recent daily OrcaVault build and may lag live OrcaBus state
> by up to roughly a day. See `docs/operations.md`.

---

## Data Account (503977275616)

Holds the bioinformatics files that htsget-rs serves.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| S3 bucket holding the bioinformatics files | elsewhere | Researcher | Indirectly via `validate`'s canary `HeadObject` (Req 15.2e). |
| Bucket policy granting `s3:GetObject`/`s3:HeadObject` to the Data Control role used by Htsget_Rs (Req 6.2) | elsewhere | Researcher | `validate`'s cross-account `HeadObject` check confirms the policy is effective (Req 15.2e). Manually: `aws s3api get-bucket-policy --bucket <data-bucket>`. |

---

## Researcher Account (per onboarded account)

Deployed and owned by that account's onboarding deployment. A cross-account manifest
read requires *both* the resource-side grant (on the Manifest_Bucket policy, owned by
Data Control) and this identity-side grant. See `docs/onboarding.md` for the full
procedure.

| Item | Owner | Prerequisite for | How to verify |
| --- | --- | --- | --- |
| Accepted AWS RAM resource share and active VPC Lattice service network association (Req 5) | Researcher onboarding | Researcher | `validate --account <id>` checks the association is active and the RAM share is accepted (Req 15.3). |
| IAM identity-based policy letting the account's own principals read the Manifest from the Data Control Manifest_Bucket | Researcher onboarding | Researcher | The resource-side grant is checked by `validate --account <id>` (Req 15.9); the identity-side policy is verified in the researcher account (`aws iam simulate-principal-policy` or an actual `s3:GetObject` from that account against `manifests/<account-id>/manifest.json`). |
| Test_Lambda (optional in-VPC probe) | htsget VPC Lattice consumer construct | Researcher (optional) | `validate --account <id>` discovers and invokes the `TestLambda`; if absent it reports the in-VPC probe is unavailable and recommends the consumer construct's `deployTestLambda` option (Req 15.4, 15.5). |

---

## Operator credentials

Data Control account credentials with the permissions enumerated in Requirement 11.2
must be available through the standard AWS credential chain (an SSO/`credential_process`
profile, environment variables, or an assumed role). `validate` confirms the credentials
resolve and carry the required permissions (Req 15.2a); see `docs/configuration.md` for
how the CLI selects the credential source.
