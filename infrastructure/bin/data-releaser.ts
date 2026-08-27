#!/usr/bin/env node
import * as path from "path";
import * as cdk from "aws-cdk-lib";
import { DataReleaserStack } from "../lib/data-releaser-stack";
import { DataShareTestStack } from "../lib/data-share-test-stack";
import { MockMartObject, MockMartStack } from "../lib/mock-mart-stack";
import { dataShareTestBucketName, TEST_DATA_OBJECTS } from "../lib/test-data";

/**
 * CDK app entrypoint for the Data Releaser stack.
 */
const app = new cdk.App();

/**
 * All configuration is under the `dataReleaser` key of cdk.context.json.
 */
interface DataReleaserContext {
  readonly account?: string;
  readonly region?: string;
  readonly namePrefix?: string;
  readonly stageName?: string;
  readonly apiVpcId?: string;
  readonly researcherAccountIds?: string[];
  readonly cacheTtlCeiling?: string;
  readonly cacheDenyTtl?: string;
  readonly provisionOperatorPolicy?: boolean;
  readonly dataShareTest?: boolean;
  readonly mockMart?: boolean;
  readonly mockMartDatabase?: string;
  readonly mockMartWorkgroup?: string;
  readonly mockMartDataBucket?: string;
  readonly mockMartCohortId?: string;
  readonly mockMartObjects?: MockMartObject[];
}

const cfg = (app.node.tryGetContext("dataReleaser") ?? {}) as DataReleaserContext;

const account = cfg.account || process.env.CDK_DEFAULT_ACCOUNT || "000000000000";
const region = cfg.region || process.env.CDK_DEFAULT_REGION || "ap-southeast-2";
const namePrefix = cfg.namePrefix ?? "data-releaser";
const apiVpcId = cfg.apiVpcId;
if (!apiVpcId) {
  throw new Error("apiVpcId is required");
}

new DataReleaserStack(app, "DataReleaserStack", {
  env: { account, region },
  namePrefix,
  stageName: cfg.stageName ?? "prod",
  researcherAccountIds: cfg.researcherAccountIds ?? [],
  cacheTtlCeiling: cfg.cacheTtlCeiling ?? "1h",
  cacheDenyTtl: cfg.cacheDenyTtl ?? "60s",
  provisionOperatorPolicy: cfg.provisionOperatorPolicy ?? false,
  apiVpcId,
  description:
    "Data Releaser Authorization Service resources in the Data Control account.",
});

// Configure test data.
const testBucketName = dataShareTestBucketName(account, region);
if (cfg.dataShareTest ?? false) {
  const sourcePath = path.join(__dirname, "..", "test-data");
  new DataShareTestStack(app, "DataShareTestStack", {
    env: { account, region },
    namePrefix,
    sourcePath,
    description: "Mock test-data bucket seeded from the htsget-rs sample data.",
  });
}

// Configure mock mart.
if (cfg.mockMart ?? false) {
  new MockMartStack(app, "MockMartStack", {
    env: { account, region },
    namePrefix,
    databaseName: cfg.mockMartDatabase ?? "mart_mock",
    workgroupName: cfg.mockMartWorkgroup ?? "data-releaser-mock",
    dataBucketName: cfg.mockMartDataBucket ?? testBucketName,
    cohortId: cfg.mockMartCohortId ?? "DEMO",
    objects: cfg.mockMartObjects ?? TEST_DATA_OBJECTS,
    description: "Mock OrcaVault Mart for Data Releaser demos (not part of Req 16).",
  });
}

app.synth();
