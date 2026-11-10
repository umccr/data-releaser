# Live AWS test runbook

End-to-end verification of Data Releaser against real AWS: deploy the
authorization service, share one library to one researcher account, read it
through htsget-rs over VPC Lattice, then prove that everything not shared stays
unreadable.

This runbook exercises the **htsget** mechanism end to end. The s3 mechanism —
which copies a dataset into this account and grants a researcher direct read
access to it — is not covered here; see [`docs/usage.md`](docs/usage.md) for its
verbs.

Every command here was checked against the code and the synthesised templates
rather than written from memory. Placeholders in `<angle brackets>` are the only
things you should have to fill in.

## Topology and what you have to create yourself

| Thing | Account | Created by |
| --- | --- | --- |
| `DataReleaserStack` (table, lambdas, API, manifest bucket) | 916218008751 (Data Control) | you, CDK |
| `DataShareTestStack` (test-data bucket + htsget-rs sample BAM) | 916218008751 | you, CDK, context `dataShareTest: true` |
| `MockMartStack` (optional Athena stand-in) | 916218008751 | you, CDK, context `mockMart: true` |
| `HtsGetLatticeData` (htsget-rs + lattice producer + RAM share) | 916218008751 | you, CDK |
| `HtsGetLatticeDemo` (VPC association + test Lambda) | 534840902377 (demo/researcher) | you, CDK |
| `UomPrimaryVpc` = `vpc-03e01fb6290012661` — hosts the htsget Lambda and the API interface endpoint | 916218008751 | already exists |
| `UomPrimaryVpc` — the researcher VPC named in `targetVpcArn` | 534840902377 | already exists |

The app also synthesises one `HotCache-<name>` stack per dataset listed under
the `datasets` context key — a bucket and a key for the s3 mechanism to copy
into. None of them is part of this runbook, and leaving `datasets` empty
deploys none.

Region throughout is `ap-southeast-2`. Both accounts need `cdk bootstrap` first.

Two different VPCs share the name `UomPrimaryVpc`, one per account, and they play
opposite roles: the **916218008751** one is where the decision is made (htsget-rs
runs there, and the authorization API is only reachable from there), the
**534840902377** one is the identity being authorized. Never substitute one ID for
the other — `vpc-03e01fb6290012661` belongs to Data Control and must never appear
in a `--target-vpc-arn`.

## API authorization, before you start

Nothing about the authorization API is reachable from the internet, and that
changes how you test it.

The REST API is a **private** endpoint. The only way in is the `execute-api`
interface endpoint the stack creates in `UomPrimaryVpc`
(`vpc-03e01fb6290012661`), and the API's resource policy allows
`execute-api:Invoke` only when `aws:SourceVpce` matches it, with an explicit deny
for everything else. On top of that:

```
/authorize            GET   network only  (unsigned — htsget-rs cannot sign)
/admin/shares         POST  AWS_IAM + network
/admin/shares         GET   AWS_IAM + network
/admin/shares/revoke  POST  AWS_IAM + network
/admin/shares/extend  POST  AWS_IAM + network
```

`/authorize` carries no credential because htsget-rs's callout config has no way
to attach one, so the private endpoint is its control. `/admin/*` additionally
requires SigV4; callers need `execute-api:Invoke` on
`arn:aws:execute-api:ap-southeast-2:916218008751:<rest-api-id>/prod/*/admin/*`,
which the operator policy grants as `InvokeAdminApi`. The CLI never calls those
routes — `share`, `list`, `revoke` and `extend` go straight to DynamoDB — so this
costs you nothing during the demo.

**What this means for testing:** you cannot curl `/authorize` from your laptop —
the hostname will not resolve. Probe it from inside the VPC instead (step 3), and
set `validation.skipAuthorizationCheck` in the CLI config so `validate` reports
that check as SKIP rather than failing on a healthy deployment.

### VPC prerequisites

`UomPrimaryVpc` in 916218008751 has to satisfy two things before any of this
works, and neither is created by these stacks:

```bash
# 1. Private subnets with egress — CDK's default selection for a VPC-attached
#    Lambda. If this returns nothing, the htsget deploy will fail at synth.
aws ec2 describe-subnets --filters Name=vpc-id,Values=vpc-03e01fb6290012661 \
  --query "Subnets[].{id:SubnetId,az:AvailabilityZone,public:MapPublicIpOnLaunch}"

# 2. A route to S3 — NAT or an S3 gateway endpoint. Without it htsget-rs
#    authorizes a request and then cannot read the object it just authorized.
aws ec2 describe-vpc-endpoints --filters Name=vpc-id,Values=vpc-03e01fb6290012661 \
  --query "VpcEndpoints[].{svc:ServiceName,type:VpcEndpointType}"
```

If there is no S3 gateway endpoint and no NAT, add the endpoint (it is free and
route-table-scoped):

```bash
aws ec2 create-vpc-endpoint --vpc-id vpc-03e01fb6290012661 \
  --vpc-endpoint-type Gateway \
  --service-name com.amazonaws.ap-southeast-2.s3 \
  --route-table-ids <the private subnets' route tables>
```

This is a shared landing-zone VPC, so `DataReleaserStack` deliberately does not do
this for you — it would be editing route tables it does not own.

## 1. Data Control infrastructure

```bash
cd infrastructure
pnpm install
pnpm run build                 # tsc; the CDK app is plain TypeScript

pnpm exec cdk deploy DataReleaserStack
```

The Go Lambdas are compiled by CDK's `GoFunction` construct during synth,
straight out of this repository's own module — there is no separate build step
and no `dist/` to populate. It needs Go on the path, or Docker if it has to fall
back to a container build.

Every setting comes from the `dataReleaser` object in
`infrastructure/cdk.context.json`, which is checked in — there are no `-c
key=value` flags to pass, because the app reads one context key and destructures
it rather than reading a key per setting. Edit that object before deploying:

```jsonc
{
  "dataReleaser": {
    "account": "916218008751",
    "region": "ap-southeast-2",
    "namePrefix": "data-releaser",
    "stageName": "prod",
    "apiVpcId": "vpc-03e01fb6290012661",
    "researcherAccountIds": ["534840902377"],
    "cacheTtlCeiling": "60s",   // demo only; the default is 1h
    "cacheDenyTtl": "30s",      // demo only; the default is 60s
    "provisionOperatorPolicy": false
  }
}
```

`apiVpcId` is required — the app throws at synth without it — and it has to be
right, because it is what makes the API private and it must be the same VPC the
htsget Lambda joins in step 4. The VPC lookup is resolved against the live
account **once** and cached in the same `cdk.context.json`, so synth works
offline as long as account/region/VPC match the cache; deploy, and a lookup for
any new account/region/VPC, need credentials for 916218008751. There is no
public fallback: the REST API is declared with
`endpointConfiguration.types = [PRIVATE]` and there is no switch that makes it
anything else.

The two short cache values are a demo convenience — with the 1h default, a revoke
can take an hour to become visible because htsget-rs caches the allow decision for
the `max-age` the Lambda emits. Use the defaults for anything real.

Set `"provisionOperatorPolicy": true` if you want the managed policy that carries
the Athena, DynamoDB, RAM, manifest and `/admin/*` invoke permissions; attach it
to the operator identity out of band (the CLI creates no IAM itself). It adds an
`OperatorPolicyArn` output.

Keep the outputs: `AuthorizationCalloutUrl`, `ApiGatewayRestApiId`,
`ApiGatewayStageName`, `ManifestBucketName`, `AuthorizationApiVpcEndpointId`.

Confirm the endpoint type and the resource policy actually landed — this is the
whole control on `/authorize`, so it is worth ten seconds:

```bash
aws apigateway get-rest-api --rest-api-id <rest-api-id> \
  --query "{types:endpointConfiguration.types,vpce:endpointConfiguration.vpcEndpointIds}"
```

Expect `types: ["PRIVATE"]` and `vpce` containing the `AuthorizationApiVpcEndpointId`.

## 2. Test data

`DataShareTestStack` creates the bucket and uploads the test BAM for you. The
sample fixtures are **vendored** in `infrastructure/test-data/` and deliberately
not git-ignored, so the stack synthesises straight from a fresh clone with
nothing to fetch:

```bash
cd infrastructure
pnpm exec cdk deploy DataShareTestStack
```

The stack is only synthesised when `"dataShareTest": true` is set in the
`dataReleaser` context object; leave it `false` and there is nothing to deploy.

This provisions `s3://data-share-test-916218008751-ap-southeast-2` (blocks all
public access, TLS-only, disposable) holding `demo/L2400001/NA12878.bam` and its
`demo/L2400001/NA12878.bam.bai` index. The index matters — region queries need
it. That bucket is also the htsget-rs static location
(`htsget-pipeline/lib/config.ts`) and the default row bucket in `MockMartStack`,
so the three line up.

The htsget id is the key **minus the bucket and minus the format extension**:
`demo/L2400001/NA12878.bam` → request path `/reads/demo/L2400001/NA12878`. This is
the single most common thing to get wrong.

If you don't have Mart access from wherever you're running the CLI:

```jsonc
// infrastructure/cdk.context.json, under "dataReleaser"
"mockMart": true,
"mockMartObjects": [{ "libraryId": "L2400001", "key": "demo/L2400001/NA12878.bam" }]
```

```bash
pnpm exec cdk deploy MockMartStack
```

It defaults to database `mart_mock`, workgroup `data-releaser-mock`, and the
test-data bucket as its row bucket — the same values the CLI config below
carries.

Never point this at an account holding the real Mart — the Glue database `mart`
would collide with the Lake Formation resource link.

## 3. Smoke-test the decision point before touching lattice

Worth doing on its own, because it isolates the Go Lambda + DynamoDB from all the
networking. Since the API is private, do it in two parts: the decision logic
directly against the Lambda (from anywhere), then the network boundary itself.

### 3a. The decision, straight to the Lambda

`aws lambda invoke` bypasses API Gateway entirely, so this works from your laptop
and tests exactly what the callout will hit — the same handler, the same
header-folding, the same DynamoDB query:

```bash
VPC=$(aws ec2 describe-vpcs --profile demo \
  --filters "Name=tag:Name,Values=UomPrimaryVpc" --query "Vpcs[0].VpcId" --output text)
ARN="arn:aws:ec2:ap-southeast-2:534840902377:vpc/$VPC"

aws lambda invoke --function-name data-releaser-authorization \
  --cli-binary-format raw-in-base64-out \
  --payload "{\"headers\":{\"Htsget-Context-SourceVpcArn\":\"$ARN\"}}" \
  /dev/stdout
```

(That `$VPC` is the **researcher's** VPC in 534840902377, not
`vpc-03e01fb6290012661`.)

With no share yet you should get `statusCode: 200`, a body whose `htsgetAuth` list
is empty, `Cache-Control: max-age=30`, and `Vary: Htsget-Context-SourceVpcArn`. A
`500` or a handler error means `SHARE_RECORDS_TABLE_NAME` is wrong or the role
can't query GSI1.

### 3b. The network boundary

From your laptop, the API must be unreachable — that is the point of it:

```bash
curl -sS --max-time 10 "<AuthorizationCalloutUrl>"
```

Expect a DNS failure (`Could not resolve host`), **not** a 403. A 403 means the
hostname resolved, so the API is somehow public — which the stack can no longer
synthesise, so treat it as tampering. A 200 means it is both public and open.

To positively confirm the endpoint works you need something with an ENI in
`vpc-03e01fb6290012661` — an SSM-managed instance in one of its private subnets is
the usual way:

```bash
# in a session on an instance in a private subnet of vpc-03e01fb6290012661
curl -sS -D- -H "Htsget-Context-SourceVpcArn: $ARN" "<AuthorizationCalloutUrl>"
```

Same 200-with-empty-list as 3a. If it hangs, the interface endpoint's security
group isn't reachable from that subnet; if it returns `403 {"Message":"User: anonymous
is not authorized to perform: execute-api:Invoke on resource: ..."}`, the request
arrived through a *different* VPC endpoint than the one in the resource policy.

If you have no instance in that VPC, skip this — step 4 verifies the same path for
real, because htsget-rs's callout is exactly this request from exactly this VPC.

### 3c. The admin routes

From inside the VPC, confirm they are still shut:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
  "https://<rest-api-id>.execute-api.ap-southeast-2.amazonaws.com/prod/admin/shares"
```

Expect `403` (`Missing Authentication Token`) — the network let it in, IAM turned
it away. Anything else means the methods lost their `AuthorizationType: AWS_IAM`.

## 4. htsget-rs producer

```bash
cd ../htsget-pipeline
pnpm install
gh release download htsget-lambda-v0.8.1 --repo umccr/htsget-rs \
  --pattern "htsget-lambda-v0.8.1-arm64.zip" --output htsget-lambda.zip

export HTSGET_LAMBDA_ARTIFACT_PATH=htsget-lambda.zip     # else CDK builds from source with cargo-lambda
export HTSGET_AUTHORIZATION_URL=$(aws cloudformation describe-stacks \
  --stack-name DataReleaserStack \
  --query "Stacks[0].Outputs[?OutputKey=='AuthorizationCalloutUrl'].OutputValue" --output text)

pnpm cdk deploy HtsGetLatticeData
```

The producer config pins `vpcOrName: "UomPrimaryVpc"`, which is what puts the
htsget Lambda inside `vpc-03e01fb6290012661` and therefore on the only network
that can reach `/authorize`. It is looked up **by name**, so if 916218008751 has
more than one VPC tagged `UomPrimaryVpc` the synth fails — resolve that before
deploying rather than letting CDK pick.

Confirm the wiring landed — both halves of it:

```bash
aws lambda get-function-configuration --function-name htsget-lattice-function \
  --query "{auth:Environment.Variables.HTSGET_AUTH_AUTHORIZATION,vpc:VpcConfig.VpcId,subnets:VpcConfig.SubnetIds}"
```

`auth` should read
`{kind=callout,url=https://…/authorize,forward={context={extensions=[{json_path=$.requestContext.identity.sourceVpcArn,name=SourceVpcArn}]}}}`
and `vpc` should be `vpc-03e01fb6290012661`. If the URL says
`htsget-authorization-url-not-configured.invalid`, the env var wasn't set and the
deployment is failing closed by design — redeploy with it set. If `vpc` is null the
function is not in the VPC and every callout will fail DNS resolution, which
htsget-rs surfaces as a blanket 403.

## 5. Consumer side

```bash
export AWS_PROFILE=demo   # 534840902377
aws ram get-resource-share-invitations \
  --query "resourceShareInvitations[?status=='PENDING'].resourceShareInvitationArn"
# accept-resource-share-invitation if anything is pending (auto-accepted within an org)

export HTSGET_SERVICE_NETWORK_ARN=$(aws vpc-lattice list-service-networks \
  --query "items[?name=='htsget-service-network'].arn | [0]" --output text)
pnpm cdk deploy HtsGetLatticeDemo
```

`HtsGetLatticeDemo` synthesises the consumer construct **only if
`HTSGET_SERVICE_NETWORK_ARN` is set** — an unset variable silently deploys an
empty stack, so check the ARN resolved before deploying.

## 6. CLI config and validate

`config.yaml`, in the Data Control account context:

```yaml
profile: data-control

ledger:
  table: data-releaser-ShareRecords   # "<namePrefix>-ShareRecords"

warehouse:
  athenaWorkgroup: data-releaser-mock  # must equal mockMartWorkgroup if using the mock
  database: mart_mock                  # must equal mockMartDatabase (mock defaults; real warehouse uses `mart`)
  catalog: AwsDataCatalog

endpoints:
  authorization: https://<rest-api-id>.execute-api.ap-southeast-2.amazonaws.com/prod/authorize
  htsgetBaseUrl: https://htsget-lattice.data-control.umccr.org
  apiGatewayRestApiId: <rest-api-id>
  apiGatewayStageName: prod

manifest:
  bucket: <ManifestBucketName>

validation:
  skipHtsgetCheck: true          # the lattice domain only resolves inside an associated VPC
  skipAuthorizationCheck: true   # the authorization API only resolves inside vpc-03e01fb6290012661

researchers:
  "534840902377":
    targetVpcArn: arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-xxxxxxxx
```

The demo exercises the htsget mechanism only, so no `s3` section is needed and
its absence blocks nothing here — each command validates only the sections it
reads.

```bash
go build -o data-releaser .
./data-releaser validate htsget --account 534840902377
```

Name the mechanism. `validate` invoked bare covers **both** mechanisms, and with
no `s3` section in the file it would report the s3 mechanism as an unconfigured
failed check and exit non-zero — an accurate answer to a question you did not
mean to ask. Naming `htsget` narrows the run to the mechanism this config
describes.

Eight provider checks print PASS/FAIL/SKIP: credentials, Athena reachability,
authorization service (SKIP), htsget health (SKIP), cross-account S3, the
DynamoDB table, the manifest bucket, and the API Gateway stage. `--account` adds
the researcher-side ones on top: the VPC Lattice / RAM association, the
researcher's cross-account read grant on the manifest bucket, and the discovery
and invocation of the test probe Lambda where one is deployed.

Both SKIPs are expected from an operator's machine and neither weakens the check —
they replace a guaranteed false failure with an honest "not verifiable from here",
and both endpoints are proven from inside the VPC by steps 3 and 7. Drop the flags
if you ever run `validate` from a host in `vpc-03e01fb6290012661`. Note that the
API Gateway stage check still runs and still passes: it uses the control-plane
`GetStage` API, which is public, not the private data plane.

## 7. Create a share and read data

```bash
./data-releaser share htsget \
  --libraries L2400001 --account 534840902377 --expiry 24h \
  --target-vpc-arn "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-xxxxxxxx"
```

The mechanism is part of the invocation and is never inferred from the flags:
`data-releaser share --libraries …` exits non-zero and names the two forms that
exist. Guessing from the flags would turn a typo into an access-control mistake
that surfaces much later as an unexplained deny.

`share htsget` refuses to run until the RAM association is `ASSOCIATED`, then
writes the record, then syncs the manifest. Re-run it with identical arguments — it should
print "Existing share reused (idempotent match)" with the same share ID.

Re-run the invoke from step 3a: it should now return the granted prefixes with a
longer `max-age`. Then the real end-to-end read, from inside the demo VPC:

```bash
export AWS_PROFILE=demo
fn=$(aws lambda list-functions \
  --query "Functions[?contains(FunctionName,'TestLambda')].FunctionName | [0]" --output text)
aws lambda invoke --function-name "$fn" --cli-binary-format raw-in-base64-out \
  --payload '{"path":"/reads/demo/L2400001/NA12878","referenceName":"11","start":4900000,"end":5100000,"format":"BAM"}' \
  response.json
cat response.json
```

A pass is `statusCode: 200` with a ticket containing presigned S3 URLs;
`curl -o part.bam "<first url>"` and check `samtools view` reads it. And the
manifest, which exercises the cross-account bucket policy:

```bash
aws s3 cp s3://<ManifestBucketName>/manifests/534840902377/manifest.json -
```

Each file should appear as `reads/demo/L2400001/NA12878` under `htsgetBaseUrl`.

## 8. Negative tests — these are the ones that prove authorization works

| Test | How | Expected |
| --- | --- | --- |
| API from the internet | `curl "<AuthorizationCalloutUrl>"` from your laptop | DNS failure — not a 403, not a 200 |
| API from an unassociated VPC | same curl from an instance in any VPC without the interface endpoint | DNS failure, or 403 `not authorized to perform: execute-api:Invoke` if it resolved via a foreign VPCE |
| Unknown VPC | step 3a invoke with a made-up VPC ARN | 200, empty `htsgetAuth`, `max-age=30` |
| No identity | step 3a invoke with `--payload '{}'` | same deny |
| Unsigned admin call | `curl -X POST .../prod/admin/shares` from inside the VPC | 403 `Missing Authentication Token` |
| Signed admin call without permission | `awscurl --service execute-api` from inside the VPC as a principal lacking `execute-api:Invoke` | 403 `not authorized to perform: execute-api:Invoke` |
| Revoked | `./data-releaser revoke htsget <share-id>`, wait past the cache TTL, re-invoke test Lambda | 403 / no ticket |
| Expired | share with `--expiry 24h`, then check after expiry (or edit `expiresAt` in the table) | 403 |
| Unshared id | request `/reads/demo/L2400002/NA12879` | 403 even though the object exists |

The last one is the important one for the data path: it proves the static
`s3://data-share-test-916218008751-ap-southeast-2` location isn't serving
everything to anyone on the service network. The first two are the important ones for the control path — they
are the difference between "the callout is unauthenticated but unreachable" and
"the callout is unauthenticated".

Caching is the usual source of confusion here — htsget-rs caches callout responses
in memory honouring `Cache-Control`, so a revoke isn't visible until the previous
allow expires (`cacheTtlCeiling`), and a Lambda cold start wipes the cache and
makes it look instantaneous.

## When it fails

| Symptom | Likely cause |
| --- | --- |
| Every request 403, step 3a invoke returns empty | `targetVpcArn` ≠ the forwarded ARN. Query GSI1 directly: `aws dynamodb query --table-name data-releaser-ShareRecords --index-name GSI1 --key-condition-expression "targetVpcArn = :v" --expression-attribute-values '{":v":{"S":"arn:aws:ec2:…"}}'` |
| Every request 403, and step 3a returns the right prefixes | htsget-rs can't reach the callout. Check `VpcConfig.VpcId` on `htsget-lattice-function` is `vpc-03e01fb6290012661`, and that the interface endpoint has `PrivateDnsEnabled: true` — without it the hostname still resolves publicly and the resource policy denies the request |
| htsget logs show a DNS or connect error on the callout URL | the Lambda is in the wrong VPC, in subnets the interface endpoint isn't in, or the endpoint SG doesn't allow 443 from the Lambda's subnet CIDR |
| `cdk deploy DataReleaserStack` fails with "Could not find any VPCs matching" | `apiVpcId` is wrong for the account/region, or you're deploying with credentials for the wrong account |
| `cdk deploy` cannot find `DataShareTestStack` or `MockMartStack` | their `dataShareTest` / `mockMart` context flags are `false`, so the app never synthesised them |
| `cdk synth` asks for credentials | the account/region/`apiVpcId` being synthesised has no cached lookup in `cdk.context.json`. Synth once with credentials to populate the cache, or restore the checked-in `dataReleaser` values so the cached entry matches |
| `cdk synth` throws `apiVpcId is required` | the `dataReleaser` context object is missing or has no `apiVpcId`. It has no default, because a wrong one silently makes the API reachable from the wrong network |
| `validate` fails the Authorization Service check | expected from outside the VPC — set `validation.skipAuthorizationCheck: true` |
| `validate` fails an `s3 mechanism` check with a list of missing config fields | you ran it bare, so it covered both mechanisms. Run `validate htsget` for this demo's config |
| 404 on a request that should work | id includes the `.bam` extension, or the object isn't in `s3://data-share-test-916218008751-ap-southeast-2` (redeploy `DataShareTestStack`) |
| htsget returns a ticket but fetching it times out | the Lambda is now in a VPC with no route to S3 — add the S3 gateway endpoint from the prerequisites |
| `share htsget` fails at preflight | RAM invitation not accepted in 534840902377 |
| `share htsget` fails on the manifest | `manifest.bucket` wrong, or the stack wasn't deployed with `researcherAccountIds` |
| `share` exits 1 saying it "requires a mechanism" | the mechanism is named as part of the invocation — `share htsget …` |
| 403 on `/admin/*` from the operator | missing `execute-api:Invoke` — deploy with `-c provisionOperatorPolicy=true` and attach `data-releaser-operator`, or sign with a principal that has it. If it isn't an IAM message, you're outside the VPC |
| Athena "No output location provided" | workgroup missing `ResultConfiguration` — the mock stack sets and enforces it; a hand-made workgroup may not |
| Test Lambda times out | VPC association missing, or `HTSGET_SERVICE_NETWORK_ARN` was unset at consumer deploy |

## Teardown

`cdk destroy` on `HtsGetLatticeDemo` → `HtsGetLatticeData` → `MockMartStack` →
`DataShareTestStack` → `DataReleaserStack`. `DataShareTestStack` empties and
deletes its bucket itself (disposable by design), so no manual S3 cleanup is
needed. The manifest bucket and the share table are retained by policy, so delete
those by hand if you want a clean slate.
