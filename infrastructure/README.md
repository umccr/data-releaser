# Data Releaser — Infrastructure_Stack (CDK)

AWS CDK **TypeScript** application that deploys, into the **Data_Control_Account**,
every AWS resource the Authorization_Service needs (Requirement 16):

| Resource                         | Notes                                                                                                                                                                                                                                            |
|----------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| DynamoDB `ShareRecords`          | PK `shareId`; GSI1 `targetVpcArn` (callout lookup); GSI2 `researcherAccountId`+`createdAt` (list/idempotency); **no TTL**                                                                                                                        |
| Authorization Lambda             | Go binary from `functions/authorization`; reads GSI1 only                                                                                                                                                                                        |
| Admin Lambda                     | Go binary from `functions/admin`; read/write table + GSI2                                                                                                                                                                                        |
| API Gateway **REST** API + stage | no gateway cache and no required request parameters — htsget-rs sends a bare `GET /authorize` with the requester identity in the `Htsget-Context-SourceVpcArn` header, and caches the response itself using the `Cache-Control` the Lambda emits |
| API reachability                 | `PRIVATE` endpoint + `execute-api` interface endpoint in `apiVpcId`, with a resource policy pinned to it — see [API authorization](#api-authorization)                                                                                           |
| API method authorization         | `/admin/*` additionally requires **SigV4** (`AuthorizationType.IAM`); `GET /authorize` is unauthenticated by necessity and relies on the private endpoint                                                                                        |
| Lambda execution roles           | scoped to the table and the single index each function queries                                                                                                                                                                                   |
| Manifest_Bucket + bucket policy  | private, block-public-access; per-account `s3:GetObject` on `manifests/<account-id>/*`                                                                                                                                                           |
| Operator IAM policy              | optional (`provisionOperatorPolicy`)                                                                                                                                                                                                             |

TypeScript (not CDK Go) because the surrounding UMCCR deployments — including the
htsget VPC Lattice constructs this system depends on — are CDK TypeScript.

The stack provisions **nothing** outside the Data Control account: no
researcher-side resources, no Lake Formation / RAM sharing, no htsget-rs
(Req 16.8).

## Parameters (Req 16.10)

Everything that varies between environments is a **parameter**, so nothing is
hardcoded and one source tree deploys to a dev and a prod Data_Control_Account
with no code change. Every parameter lives under the `dataReleaser` key of
[`cdk.context.json`](cdk.context.json); edit that file to retarget a deployment.
No `-c` flags are read, and `cdk.json` carries only CDK feature flags.

`cdk.context.json` rather than `cdk.json` or `-c` for two reasons. `-c` can only
carry strings — `cdk`'s help calls it a "contextual *string* parameter", so
`-c provisionOperatorPolicy=false` arrives as the string `"false"`, which is
truthy; every boolean, number and list would need a coercion layer whose bugs
silently deploy the opposite of what was asked. And `cdk.json` context takes
precedence over `cdk.context.json` for the same key, so the block must live in
exactly one of them or the wrong one silently wins.

> ⚠️ `cdk context --clear` / `--reset` rewrite the whole file and **delete the
> hand-authored `dataReleaser` block** along with the lookup cache. Restore it
> with `git checkout cdk.context.json`, or remove just the `vpc-provider:` entry
> by hand instead.

| Context key               | Meaning                                                                                                               | Default                                     |
|---------------------------|-----------------------------------------------------------------------------------------------------------------------|---------------------------------------------|
| `account`                 | Target AWS account                                                                                                    | `CDK_DEFAULT_ACCOUNT`, else placeholder     |
| `region`                  | Target region                                                                                                         | `CDK_DEFAULT_REGION`, else `ap-southeast-2` |
| `namePrefix`              | Physical-name prefix                                                                                                  | `data-releaser`                             |
| `stageName`               | REST API stage name                                                                                                   | `prod`                                      |
| `researcherAccountIds`    | Accounts granted manifest read (JSON array of account IDs)                                                             | `[]`                                        |
| `cacheTtlCeiling`         | Cache_TTL ceiling (Go duration)                                                                                       | `1h`                                        |
| `cacheDenyTtl`            | Negative-cache max-age (Go duration)                                                                                  | `60s`                                       |
| `encryption`              | Manifest_Bucket SSE: `s3-managed` or `kms`                                                                            | `s3-managed`                                |
| `provisionOperatorPolicy` | Provision the Req 11.2 operator policy                                                                                | `false`                                     |
| `apiVpcId`                | VPC hosting the API's interface endpoint and every Lambda; must be the VPC the htsget-rs Lambda runs in. **Required** | `vpc-03e01fb6290012661`                     |

Example — deploy to production Data Control, granting two researcher accounts
read on their own manifest prefix:

```jsonc
// cdk.context.json
{
  "dataReleaser": {
    "account": "916218008751",
    "region": "ap-southeast-2",
    "apiVpcId": "vpc-03e01fb6290012661",
    "researcherAccountIds": ["111111111111", "222222222222"]
  }
}
```

```bash
pnpm install
cdk deploy
```

## Go Lambda bundling

Both Lambdas are **Go** binaries on the `provided.al2023` custom runtime, targeting
**arm64/Graviton**. `GoFunction` from `@aws-cdk/aws-lambda-go-alpha` compiles them
as part of `cdk synth`, so there is no separate build step and nothing to rebuild
by hand. It emits the executable as `bootstrap`, which the custom runtime requires.

Per function this is equivalent to:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o bootstrap ./functions/<fn>
```

`GOOS`, `GOARCH` (from the function's `architecture`), and `CGO_ENABLED=0` are the
construct's own defaults; `-trimpath -ldflags "-s -w"` comes from
`bundling.goBuildFlags`. `moduleDir` points at the repo-root `go.mod`, since the Go
module spans the whole repo rather than `infrastructure/`.

Bundling uses the local Go toolchain when one is on `PATH` and falls back to a
Docker container otherwise, so Go **or** Docker is enough. The asset hash is
derived from the compiled binary, so editing a Go source file invalidates the
asset and triggers a rebuild on the next synth.

> `@aws-cdk/aws-lambda-go-alpha` is an **alpha** CDK module: its API is outside
> semver and may change or be removed in a future release. It is pinned to
> `2.160.0-alpha.0`, matching `aws-cdk-lib`, and both must be bumped together.

## Optional: mock test-data bucket (`DataShareTestStack`)

In the real system the shareable reads live in a Data_Account bucket outside this
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

## Optional: mock OrcaVault Mart (`MockMartStack`)

A second, **optional** stack stands in for the OrcaVault Mart so the CLI can be
demoed or exercised end-to-end without warehouse access. It is not part of
Req 16 and is **off by default**; it deploys only when
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
the htsget-rs Lambda is attached to via `vpcOrName` in htsget-pipeline. The stack
looks that VPC up rather than creating it, and touches nothing in it beyond the
interface endpoint and security groups (the endpoint's allows 443 from the VPC
CIDR; the Lambdas' allows 443 egress only). It does
**not** add an S3 gateway endpoint: the VPC needs its own route to S3 (NAT or a
gateway endpoint) or htsget-rs will authorize requests it cannot then serve.

There is deliberately **no public escape hatch**: the former `privateApi=false`
context flag has been removed, and `lib/vpc-only-aspect.ts` fails `cdk synth`
for any Lambda without a VPC, any non-PRIVATE or policy-less REST API, any
API Gateway v2 API, any Lambda function URL or `*`-principal permission, and
any bucket not blocking all public access. Making anything public again
requires editing that aspect — a reviewable code change, not a context value.

The VPC lookup is resolved against the real account **once** and cached in
`cdk.context.json` (checked in), beside the `dataReleaser` configuration block,
so `cdk synth` works with no credentials. Only a lookup for a new
account/region/VPC needs credentials to populate the cache.

## Stack outputs → CLI config (Req 16.4)

| Output | CLI config field |
| --- | --- |
| `ApiGatewayRestApiId` | `endpoints.apiGatewayRestApiId` |
| `ApiGatewayStageName` | `endpoints.apiGatewayStageName` |
| `ManifestBucketName` | `manifest.bucket` |
| `AuthorizationCalloutUrl` | `endpoints.authorization` |
| `AuthorizationApiVpcEndpointId` | — (the only route to the API) |

## Commands

```bash
pnpm install            # install CDK dependencies (respects the lockfile)
pnpm run build          # tsc typecheck/compile
pnpm test               # CDK assertion tests over the synthesised template
pnpm exec cdk synth     # synthesise; no flags, and no AWS credentials needed
                        # when dataReleaser.account/region in cdk.context.json
                        # match the VPC lookup cached in the same file
```
