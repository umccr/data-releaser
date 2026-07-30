import * as cdk from "aws-cdk-lib";
import { Template, Match } from "aws-cdk-lib/assertions";
import { DataReleaserStack } from "../lib/data-releaser-stack";

/**
 * CDK assertion tests over the synthesised template (task 22.7, Req 16.2/3/4/5/7).
 * These do not require AWS credentials and use dummy asset directories provided
 * by the test setup (the lambda code assets need only exist as directories).
 */

const RESEARCHER_A = "111111111111";
const RESEARCHER_B = "222222222222";

function synth(overrides: Partial<Parameters<typeof buildStack>[0]> = {}) {
  return buildStack({
    encryption: "s3-managed",
    provisionOperatorPolicy: false,
    researcherAccountIds: [RESEARCHER_A, RESEARCHER_B],
    ...overrides,
  });
}

function buildStack(opts: {
  encryption: "s3-managed" | "kms";
  provisionOperatorPolicy: boolean;
  researcherAccountIds: string[];
}): Template {
  const app = new cdk.App();
  const stack = new DataReleaserStack(app, "TestStack", {
    env: { account: "000000000000", region: "ap-southeast-2" },
    namePrefix: "data-releaser",
    stageName: "prod",
    researcherAccountIds: opts.researcherAccountIds,
    cacheTtlCeiling: "1h",
    cacheDenyTtl: "60s",
    encryption: opts.encryption,
    provisionOperatorPolicy: opts.provisionOperatorPolicy,
  });
  return Template.fromStack(stack);
}

describe("Share_Record table (Req 16.2, 8.2)", () => {
  const t = synth();

  it("has partition key shareId and both GSIs", () => {
    t.hasResourceProperties("AWS::DynamoDB::Table", {
      KeySchema: [{ AttributeName: "shareId", KeyType: "HASH" }],
      GlobalSecondaryIndexes: Match.arrayWith([
        Match.objectLike({
          IndexName: "GSI1",
          KeySchema: [{ AttributeName: "targetVpcArn", KeyType: "HASH" }],
        }),
        Match.objectLike({
          IndexName: "GSI2",
          KeySchema: [
            { AttributeName: "researcherAccountId", KeyType: "HASH" },
            { AttributeName: "createdAt", KeyType: "RANGE" },
          ],
        }),
      ]),
    });
  });

  it("GSI1 projects only the attributes the callout reads", () => {
    t.hasResourceProperties("AWS::DynamoDB::Table", {
      GlobalSecondaryIndexes: Match.arrayWith([
        Match.objectLike({
          IndexName: "GSI1",
          Projection: {
            ProjectionType: "INCLUDE",
            NonKeyAttributes: Match.arrayWith([
              "s3Prefixes",
              "expiresAt",
              "revokedAt",
              "createdAt",
            ]),
          },
        }),
      ]),
    });
  });

  it("configures NO TTL (records must remain visible to list/audit)", () => {
    const tables = t.findResources("AWS::DynamoDB::Table");
    for (const key of Object.keys(tables)) {
      const props = tables[key].Properties ?? {};
      // TimeToLiveSpecification must be absent, or present but disabled.
      if (props.TimeToLiveSpecification) {
        expect(props.TimeToLiveSpecification.Enabled).toBe(false);
      } else {
        expect(props.TimeToLiveSpecification).toBeUndefined();
      }
    }
  });
});

describe("API Gateway (Req 16.3)", () => {
  const t = synth();

  it("is a REST API, not an HTTP API", () => {
    t.resourceCountIs("AWS::ApiGateway::RestApi", 1);
    t.resourceCountIs("AWS::ApiGatewayV2::Api", 0);
  });

  it("does NOT enable a response cache on the stage", () => {
    const stages = t.findResources("AWS::ApiGateway::Stage");
    expect(Object.keys(stages)).toHaveLength(1);
    const props = Object.values(stages)[0].Properties ?? {};

    // No provisioned cache cluster, and no per-method caching opt-in. Authorization
    // response caching lives in the htsget-rs callout client's HTTP cache instead.
    expect(props.CacheClusterEnabled).toBeFalsy();
    expect(props.CacheClusterSize).toBeUndefined();
    for (const setting of props.MethodSettings ?? []) {
      expect(setting.CachingEnabled).toBeFalsy();
      expect(setting.CacheTtlInSeconds).toBeUndefined();
    }
  });

  it("requires the requester identity (sourceVpcArn) on the callout method", () => {
    t.hasResourceProperties("AWS::ApiGateway::Method", {
      HttpMethod: "GET",
      RequestParameters: { "method.request.querystring.sourceVpcArn": true },
    });

    // No integration declares cache key parameters — there is no gateway cache.
    const methods = t.findResources("AWS::ApiGateway::Method");
    for (const key of Object.keys(methods)) {
      const integration = methods[key].Properties?.Integration ?? {};
      expect(integration.CacheKeyParameters).toBeUndefined();
    }
  });
});

describe("Manifest_Bucket (Req 16.5, 7.9)", () => {
  const t = synth();

  it("blocks all public access", () => {
    t.hasResourceProperties("AWS::S3::Bucket", {
      PublicAccessBlockConfiguration: {
        BlockPublicAcls: true,
        BlockPublicPolicy: true,
        IgnorePublicAcls: true,
        RestrictPublicBuckets: true,
      },
    });
  });

  it("grants each researcher GetObject scoped to its own prefix and nothing else", () => {
    t.hasResourceProperties("AWS::S3::BucketPolicy", {
      PolicyDocument: {
        Statement: Match.arrayWith([
          Match.objectLike({
            Effect: "Allow",
            Action: "s3:GetObject",
            Principal: { AWS: Match.anyValue() },
            Resource: Match.anyValue(),
          }),
        ]),
      },
    });

    // Verify the per-account scoping: one GetObject statement per account, each
    // resource ARN ending in that account's prefix.
    const policies = t.findResources("AWS::S3::BucketPolicy");
    const doc = Object.values(policies)[0].Properties.PolicyDocument;
    const getObjectStmts = doc.Statement.filter(
      (s: any) => s.Action === "s3:GetObject",
    );
    expect(getObjectStmts).toHaveLength(2);
    const asString = JSON.stringify(getObjectStmts);
    expect(asString).toContain(`manifests/${RESEARCHER_A}/*`);
    expect(asString).toContain(`manifests/${RESEARCHER_B}/*`);
    // No researcher statement grants anything beyond GetObject.
    for (const s of getObjectStmts) {
      expect(s.Action).toBe("s3:GetObject");
    }
  });
});

describe("Execution roles are least-privilege (Req 16.7)", () => {
  const t = synth();

  it("authorization role queries GSI1 only, no table-wide or GSI2 access", () => {
    const policies = t.findResources("AWS::IAM::Policy");
    const authPolicy = Object.values(policies).find((p: any) =>
      JSON.stringify(p.Properties.PolicyDocument).includes("QueryGsi1Only"),
    ) as any;
    expect(authPolicy).toBeDefined();
    const stmts = authPolicy.Properties.PolicyDocument.Statement;
    const query = stmts.find((s: any) => s.Sid === "QueryGsi1Only");
    expect(query.Action).toBe("dynamodb:Query");
    const serialized = JSON.stringify(query.Resource);
    expect(serialized).toContain("index/GSI1");
    expect(serialized).not.toContain("index/GSI2");
    // No wildcard dynamodb:* anywhere in the authorization policy.
    expect(JSON.stringify(stmts)).not.toContain("dynamodb:*");
  });

  it("admin role reads/writes the table and queries GSI2, never GSI1 or dynamodb:*", () => {
    const policies = t.findResources("AWS::IAM::Policy");
    const adminPolicy = Object.values(policies).find((p: any) =>
      JSON.stringify(p.Properties.PolicyDocument).includes("QueryGsi2Only"),
    ) as any;
    expect(adminPolicy).toBeDefined();
    const serialized = JSON.stringify(adminPolicy.Properties.PolicyDocument);
    expect(serialized).toContain("index/GSI2");
    expect(serialized).not.toContain("index/GSI1");
    expect(serialized).not.toContain("dynamodb:*");
  });
});

describe("Stack outputs (Req 16.4)", () => {
  const t = synth();
  it("exports the three CLI config values", () => {
    const outputs = t.findOutputs("*");
    const names = Object.keys(outputs);
    expect(names).toEqual(
      expect.arrayContaining([
        "ApiGatewayRestApiId",
        "ApiGatewayStageName",
        "ManifestBucketName",
      ]),
    );
  });
});

describe("Optional operator policy (Req 16.9)", () => {
  it("is absent by default", () => {
    const t = synth({ provisionOperatorPolicy: false });
    const managed = t.findResources("AWS::IAM::ManagedPolicy");
    expect(Object.keys(managed)).toHaveLength(0);
  });
  it("is provisioned when requested", () => {
    const t = synth({ provisionOperatorPolicy: true });
    t.resourceCountIs("AWS::IAM::ManagedPolicy", 1);
  });
  it("grants no API Gateway cache-flush permission", () => {
    const t = synth({ provisionOperatorPolicy: true });
    const managed = t.findResources("AWS::IAM::ManagedPolicy");
    const doc = JSON.stringify(Object.values(managed)[0].Properties.PolicyDocument);
    expect(doc).not.toContain("apigateway:POST");
    expect(doc).not.toContain("/cache");
  });
});

describe("KMS encryption option (Req 16.5)", () => {
  it("creates a key with per-account decrypt grants when encryption=kms", () => {
    const t = synth({ encryption: "kms" });
    t.resourceCountIs("AWS::KMS::Key", 1);
    const keys = t.findResources("AWS::KMS::Key");
    const doc = JSON.stringify(Object.values(keys)[0].Properties.KeyPolicy);
    expect(doc).toContain("kms:Decrypt");
  });
});
