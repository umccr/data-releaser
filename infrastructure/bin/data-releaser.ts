#!/usr/bin/env node
import * as cdk from "aws-cdk-lib";
import { DataReleaserStack, EncryptionMode } from "../lib/data-releaser-stack";

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
 */
const app = new cdk.App();

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

// Account/region: explicit context wins, then the profile-derived CDK_DEFAULT_*
// env vars, then placeholders so credential-free `cdk synth` still works.
const account = asString("account", process.env.CDK_DEFAULT_ACCOUNT || "000000000000");
const region = asString("region", process.env.CDK_DEFAULT_REGION || "ap-southeast-2");

const encryptionRaw = asString("encryption", "s3-managed").toLowerCase();
const encryption: EncryptionMode = encryptionRaw === "kms" ? "kms" : "s3-managed";

new DataReleaserStack(app, "DataReleaserStack", {
  env: { account, region },
  namePrefix: asString("namePrefix", "data-releaser"),
  stageName: asString("stageName", "prod"),
  researcherAccountIds: asAccountList("researcherAccountIds"),
  cacheTtlCeiling: asString("cacheTtlCeiling", "1h"),
  cacheDenyTtl: asString("cacheDenyTtl", "60s"),
  encryption,
  provisionOperatorPolicy: asBoolean("provisionOperatorPolicy", false),
  description:
    "Data Releaser Authorization Service resources in the Data Control account (Req 16).",
});

app.synth();
