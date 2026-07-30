# Data Releaser — Infrastructure_Stack (CDK)

AWS CDK **TypeScript** application that deploys, into the **Data_Control_Account**,
every AWS resource the Authorization_Service needs (Requirement 16):

| Resource | Notes |
| --- | --- |
| DynamoDB `ShareRecords` | PK `shareId`; GSI1 `targetVpcArn` (callout lookup); GSI2 `researcherAccountId`+`createdAt` (list/idempotency); **no TTL** |
| Authorization Lambda | Go binary from `functions/authorization`; reads GSI1 only |
| Admin Lambda | Go binary from `functions/admin`; read/write table + GSI2 |
| API Gateway **REST** API + stage | response caching enabled; cache key includes the forwarded requester identity |
| Lambda execution roles | scoped to the table and the single index each function queries |
| Manifest_Bucket + bucket policy | private, block-public-access; per-account `s3:GetObject` on `manifests/<account-id>/*` |
| Operator IAM policy | optional (`provisionOperatorPolicy`) |

TypeScript (not CDK Go) because the surrounding UMCCR deployments — including the
htsget VPC Lattice constructs this system depends on — are CDK TypeScript.

The stack provisions **nothing** outside the Data Control account: no
researcher-side resources, no Lake Formation / RAM sharing, no htsget-rs
(Req 16.8).

## Parameters (Req 16.10)

Everything that varies between environments is a **parameter**, supplied through
**CDK context** — nothing is hardcoded, so one source tree deploys to a dev and a
prod Data_Control_Account with no code change. Defaults live in `cdk.json` under
the `dataReleaser` key; override per invocation with `-c key=value`.

| Context key | Meaning | Default |
| --- | --- | --- |
| `account` | Target AWS account | `CDK_DEFAULT_ACCOUNT`, else placeholder |
| `region` | Target region | `CDK_DEFAULT_REGION`, else `ap-southeast-2` |
| `namePrefix` | Physical-name prefix | `data-releaser` |
| `stageName` | REST API stage name | `prod` |
| `researcherAccountIds` | Accounts granted manifest read (JSON array or comma list) | `[]` |
| `cacheTtlCeiling` | Cache_TTL ceiling (Go duration) | `1h` |
| `cacheDenyTtl` | Negative-cache max-age (Go duration) | `60s` |
| `cacheClusterSize` | API Gateway cache cluster size | `0.5` |
| `encryption` | Manifest_Bucket SSE: `s3-managed` or `kms` | `s3-managed` |
| `provisionOperatorPolicy` | Provision the Req 11.2 operator policy | `false` |

Example — deploy to production Data Control:

```bash
npm install
npm run build:lambdas          # cross-compiles the Go Lambdas (see below)
cdk deploy \
  -c account=916218008751 -c region=ap-southeast-2 \
  -c researcherAccountIds=111111111111,222222222222
```

## Go Lambda bundling

Both Lambdas are **Go** binaries on the `provided.al2023` custom runtime, targeting
**arm64/Graviton**. AWS custom runtimes require the executable to be named
`bootstrap`. `scripts/build-lambdas.sh` (run via `npm run build:lambdas`)
cross-compiles each entrypoint:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/<fn>/bootstrap ./functions/<fn>
```

The stack references `dist/authorization/` and `dist/admin/` as `lambda.Code.fromAsset`
targets. `dist/` is git-ignored and must be rebuilt before `cdk synth`/`cdk deploy`.

## Stack outputs → CLI config (Req 16.4)

| Output | CLI config field |
| --- | --- |
| `ApiGatewayRestApiId` | `endpoints.apiGatewayRestApiId` |
| `ApiGatewayStageName` | `endpoints.apiGatewayStageName` |
| `ManifestBucketName` | `manifest.bucket` |
| `AuthorizationCalloutUrl` | `endpoints.authorization` |

## Commands

```bash
npm install            # install CDK dependencies
npm run build:lambdas  # cross-compile the Go Lambdas into dist/
npm run build          # tsc typecheck/compile
npm test               # CDK assertion tests over the synthesised template
npx cdk synth          # synthesise (works with no AWS credentials)
```
