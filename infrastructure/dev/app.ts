#!/usr/bin/env node
import * as path from "path";
import * as cdk from "aws-cdk-lib";
import { DataReleaserStack } from "../src/data-releaser-stack";
import { DataShareTestStack } from "./data-share-test-stack";
import { DatasetConfig, HotCacheStacks } from "../src/hot-cache-stacks";
import { MockMartObject, MockMartStack } from "./mock-mart-stack";
import { dataShareTestBucketName, TEST_DATA_OBJECTS } from "./test-data";

/**
 * Development app for this repository, configured from cdk.context.json (see
 * cdk.context.example.json). It also holds the demo-only stacks.
 *
 * Real deployments use the published `@umccr/data-releaser-cdk` package from their
 * own CDK app instead; see README.md.
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
  readonly datasets?: DatasetConfig[];
  readonly copyWriterRoleName?: string;
  readonly copyWriterRoleAccount?: string;
  readonly copyStateMachineArn?: string;
  readonly copyWorkingBucketName?: string;
  readonly custodianRoleArn?: string;
  readonly obscureBucketNames?: boolean;
  readonly dataRetentionDays?: number;
  readonly keyPendingWindowDays?: number;
  readonly logObjectAccess?: boolean;
  readonly accessLogsBucketName?: string;
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
  // Both belong to the separately deployed copy orchestration. Given, the stack
  // also deploys the function recording a finished dataset copy.
  copyStateMachineArn: cfg.copyStateMachineArn,
  copyWorkingBucketName: cfg.copyWorkingBucketName,
});

// One bucket and one key per released dataset, each in a stack of its own so
// that destroying one dataset's copy cannot reach another's.
const datasets = cfg.datasets ?? [];
HotCacheStacks.forDatasets(app, {
  env: { account, region },
  namePrefix,
  datasets,
  copyWriterRoleName: cfg.copyWriterRoleName,
  copyWriterRoleAccount: cfg.copyWriterRoleAccount,
  custodianRoleArn: cfg.custodianRoleArn,
  obscureBucketNames: cfg.obscureBucketNames,
  dataRetentionDays: cfg.dataRetentionDays,
  keyPendingWindowDays: cfg.keyPendingWindowDays,
  logObjectAccess: cfg.logObjectAccess,
  accessLogsBucketName: cfg.accessLogsBucketName,
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
    description:
      "Mock OrcaVault Mart for Data Releaser demos; stands in for the warehouse and is never deployed beside it.",
  });
}

app.synth();
