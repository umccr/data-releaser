#!/usr/bin/env node
import * as fs from "fs";
import * as path from "path";
import * as cdk from "aws-cdk-lib";
import { DataReleaserStack, EncryptionMode } from "../lib/data-releaser-stack";
import { DataShareTestStack } from "../lib/data-share-test-stack";
import { MockMartObject, MockMartStack } from "../lib/mock-mart-stack";
import { dataShareTestBucketName, TEST_DATA_OBJECTS } from "../lib/test-data";
import { VpcOnlyAspect } from "../lib/vpc-only-aspect";

/**
 * CDK app entrypoint for the Data Releaser Infrastructure_Stack (Requirement 16).
 *
 * Everything that varies between environments — account, region, resource-name
 * prefix, the set of researcher accounts granted manifest read, the cache
 * max-age ceiling and negative-cache TTL, the manifest encryption mode, and
 * whether the optional operator IAM policy is provisioned — is a PARAMETER,
 * never hardcoded (Req 16.10). Parameters are supplied through CDK context, so
 * one source tree deploys to a development and a production
 * Data_Control_Account with no code change:
 *
 *   # deploy to the production Data Control account
 *   cdk deploy \
 *     -c account=916218008751 -c region=ap-southeast-2 \
 *     -c researcherAccountIds=111111111111,222222222222
 *
 * Defaults for the non-identity parameters live in cdk.json under the
 * "dataReleaser" context key; per-invocation `-c key=value` flags override them.
 * `account`/`region` fall back to the standard CDK_DEFAULT_* environment
 * variables (populated from the active AWS profile) when not given explicitly,
 * and to placeholder values so that `cdk synth` works with no AWS credentials.
 *
 * One exception to the credential-free synth: `apiVpcId` is looked up, and a
 * VPC lookup is resolved by the CLI against the real account (with the result
 * cached in cdk.context.json, so synth is credential-free once the lookup has
 * run once). There is deliberately NO public fallback: the former
 * `privateApi=false` escape hatch has been removed, and the VpcOnlyAspect
 * below fails synth on any non-private API or non-VPC Lambda. The assertion
 * tests get CDK's dummy VPC instead of a real lookup.
 */
const app = new cdk.App();

// App-wide backstop: any stack added to this app — including future ones —
// fails synth if it contains a Lambda outside the VPC, a non-PRIVATE API
// Gateway, an HTTP API, a function URL, a public Lambda permission, or a
// bucket without a full public-access block. DataReleaserStack additionally
// applies the same aspect to itself so the guarantee holds for consumers that
// bypass this entrypoint.
cdk.Aspects.of(app).add(new VpcOnlyAspect());

const defaults = (app.node.tryGetContext("dataReleaser") ?? {}) as Record<string, unknown>;

/** Read a context key, falling back to the cdk.json "dataReleaser" block. */
function ctx(key: string): unknown {
  const direct = app.node.tryGetContext(key);
  if (direct !== undefined && direct !== null && direct !== "") {
    return direct;
  }
  return defaults[key];
}

function asString(key: string, fallback?: string): string {
  const v = ctx(key);
  if (v === undefined || v === null || v === "") {
    if (fallback !== undefined) return fallback;
    return "";
  }
  return String(v);
}

function asBoolean(key: string, fallback: boolean): boolean {
  const v = ctx(key);
  if (v === undefined || v === null || v === "") return fallback;
  if (typeof v === "boolean") return v;
  return String(v).toLowerCase() === "true";
}

/**
 * Researcher accounts may be provided either as a JSON array (from cdk.json) or
 * as a comma-separated string (convenient for `-c researcherAccountIds=a,b`).
 * The list may be empty: the stack then deploys the Manifest_Bucket with no
 * cross-account grants, and onboarding a researcher later is a pure
 * configuration change (Req 16.5).
 */
function asAccountList(key: string): string[] {
  const v = ctx(key);
  if (v === undefined || v === null || v === "") return [];
  if (Array.isArray(v)) return v.map((x) => String(x).trim()).filter((x) => x.length > 0);
  return String(v)
    .split(",")
    .map((x) => x.trim())
    .filter((x) => x.length > 0);
}

/**
 * Mock Mart rows, given as `libraryId=key` pairs — either a JSON array in
 * cdk.json or a comma-separated string for `-c mockMartObjects=L1=a/b.bam,...`.
 */
function asMockMartObjects(key: string, fallback: MockMartObject[]): MockMartObject[] {
  const v = ctx(key);
  if (v === undefined || v === null || v === "") return fallback;

  const entries = Array.isArray(v) ? v.map((x) => String(x)) : String(v).split(",");
  return entries
    .map((entry) => entry.trim())
    .filter((entry) => entry.length > 0)
    .map((entry) => {
      const separator = entry.indexOf("=");
      if (separator === -1) {
        throw new Error(`mockMartObjects entry ${JSON.stringify(entry)} must be libraryId=key`);
      }
      const libraryId = entry.slice(0, separator).trim();
      const objectKey = entry.slice(separator + 1).trim();
      if (libraryId === "" || objectKey === "") {
        throw new Error(`mockMartObjects entry ${JSON.stringify(entry)} must be libraryId=key`);
      }
      return { libraryId, key: objectKey };
    });
}

// Account/region: explicit context wins, then the profile-derived CDK_DEFAULT_*
// env vars, then placeholders so credential-free `cdk synth` still works.
const account = asString("account", process.env.CDK_DEFAULT_ACCOUNT || "000000000000");
const region = asString("region", process.env.CDK_DEFAULT_REGION || "ap-southeast-2");

const encryptionRaw = asString("encryption", "s3-managed").toLowerCase();
const encryption: EncryptionMode = encryptionRaw === "kms" ? "kms" : "s3-managed";

// The public escape hatch is gone. Fail loudly — rather than silently ignoring
// the flag — if a deploy script still passes it, so nobody believes they
// deployed a public sandbox when they actually got a private API, or worse,
// expects the flag to still be able to make the API public.
if (ctx("privateApi") !== undefined) {
  throw new Error(
    "The privateApi context flag has been removed: the authorization API is " +
      "always PRIVATE and cannot be deployed publicly. Remove privateApi from " +
      "cdk.json / -c flags. (VpcOnlyAspect additionally fails synth on any " +
      "public endpoint.)",
  );
}

const apiVpcId = asString("apiVpcId");
if (apiVpcId === "") {
  throw new Error(
    "apiVpcId is required: the authorization API is always PRIVATE and needs " +
      "an execute-api interface endpoint in the VPC the htsget-rs Lambda runs " +
      "in, and both Lambda functions attach to that VPC. Pass -c apiVpcId=vpc-xxxx " +
      "or set it in cdk.json under dataReleaser.",
  );
}

new DataReleaserStack(app, "DataReleaserStack", {
  env: { account, region },
  namePrefix: asString("namePrefix", "data-releaser"),
  stageName: asString("stageName", "prod"),
  researcherAccountIds: asAccountList("researcherAccountIds"),
  cacheTtlCeiling: asString("cacheTtlCeiling", "1h"),
  cacheDenyTtl: asString("cacheDenyTtl", "60s"),
  encryption,
  provisionOperatorPolicy: asBoolean("provisionOperatorPolicy", false),
  // The callout cannot authenticate itself, so the API is ALWAYS private and
  // reachable only from the VPC the htsget-rs Lambda runs in. `apiVpcId` is
  // looked up at synth time, which means this parameter — unlike the others —
  // needs credentials for the first VPC context lookup.
  apiVpcId,
  description:
    "Data Releaser Authorization Service resources in the Data Control account (Req 16).",
});

// Name of the mock test-data bucket, derived from the deploy target so it is
// unique in the global S3 namespace. It is both the bucket DataShareTestStack
// creates and, by default, the bucket the mock Mart advertises its rows against.
const testBucketName = dataShareTestBucketName(account, region);

// Optional stand-in for the Data_Account data bucket, off by default. In the
// real system the shareable reads live in a Data_Account outside this repo; for
// the mock/demo set-up this stack stands up a locked-down, disposable bucket
// seeded with the public htsget-rs sample data. Deploy after running
// `npm run fetch:test-data`:
//
//   npm run fetch:test-data
//   cdk deploy DataShareTestStack -c dataShareTest=true \
//     -c account=916218008751 -c region=ap-southeast-2
if (asBoolean("dataShareTest", false)) {
  const sourcePath = path.join(__dirname, "..", "test-data");

  // Fail fast with a pointer to the fetch script rather than synthesising a
  // bucket that silently 404s in htsget because a fixture is missing.
  const missing = TEST_DATA_OBJECTS.map((o) => o.key).filter(
    (key) => !fs.existsSync(path.join(sourcePath, key)),
  );
  if (!fs.existsSync(sourcePath) || missing.length > 0) {
    throw new Error(
      `test-data fixtures are missing (${missing.join(", ") || sourcePath}). ` +
        "Run `npm run fetch:test-data` to download them from the htsget-rs test " +
        "data repo before synth/deploy of DataShareTestStack.",
    );
  }

  new DataShareTestStack(app, "DataShareTestStack", {
    env: { account, region },
    namePrefix: asString("namePrefix", "data-releaser"),
    sourcePath,
    description: "Mock test-data bucket seeded from the htsget-rs sample data.",
  });
}

// Optional stand-in for the OrcaVault Mart, off by default. It is not part of
// Req 16. Its Glue database and Athena workgroup default to mock-specific names
// (`mart_mock` / `data-releaser-mock`) so it can coexist with the real
// warehouse's Lake Formation `mart` resource link and workgroup in the same
// account — reusing those names would collide in the shared Glue catalog. Its
// rows point at the DataShareTestStack bucket by default, so the two mock stacks
// are meant to be deployed together:
//
//   cdk deploy MockMartStack -c mockMart=true \
//     -c mockMartObjects=L2400001=demo/L2400001/NA12878.bam
if (asBoolean("mockMart", false)) {
  new MockMartStack(app, "MockMartStack", {
    env: { account, region },
    namePrefix: asString("namePrefix", "data-releaser"),
    // These two must equal warehouse.database / warehouse.athenaWorkgroup in
    // the CLI config: the resolution query is `FROM <database>.bam`, run in the
    // named workgroup. Both are required CLI config fields with no default.
    databaseName: asString("mockMartDatabase", "mart_mock"),
    workgroupName: asString("mockMartWorkgroup", "data-releaser-mock"),
    // Defaults to the DataShareTestStack bucket so the advertised rows resolve
    // to objects that actually exist; override to point at a different bucket.
    dataBucketName: asString("mockMartDataBucket", testBucketName),
    cohortId: asString("mockMartCohortId", "DEMO"),
    objects: asMockMartObjects("mockMartObjects", TEST_DATA_OBJECTS),
    description: "Mock OrcaVault Mart for Data Releaser demos (not part of Req 16).",
  });
}

app.synth();
