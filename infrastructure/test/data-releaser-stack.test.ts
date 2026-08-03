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
    // Vpc.fromLookup resolves to CDK's dummy VPC without a real context lookup,
    // which is enough to synthesise the endpoint, the resource policy and the
    // Lambda VpcConfigs.
    apiVpcId: "vpc-12345",
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

  it("does not require any request parameter on the callout method", () => {
    // htsget-rs sends the callout as a bare GET with the identity in the
    // Htsget-Context-SourceVpcArn header. A required query-string parameter would
    // make API Gateway reject every callout with a 400 before the Lambda runs.
    const methods = t.findResources("AWS::ApiGateway::Method");
    for (const key of Object.keys(methods)) {
      const params = methods[key].Properties?.RequestParameters ?? {};
      for (const name of Object.keys(params)) {
        expect(name.startsWith("method.request.querystring.")).toBe(false);
      }

      // No integration declares cache key parameters — there is no gateway cache.
      const integration = methods[key].Properties?.Integration ?? {};
      expect(integration.CacheKeyParameters).toBeUndefined();
    }
  });

  it("requires SigV4 on every admin method and leaves only the callout open", () => {
    // /admin/* creates, revokes and extends shares. Unauthenticated, anyone who
    // finds the invoke URL could grant themselves access to the data bucket.
    const methods = t.findResources("AWS::ApiGateway::Method");
    const resources = t.findResources("AWS::ApiGateway::Resource");

    // Map logical id -> path part so a method can be attributed to a path.
    const pathPart = (ref: any): string =>
      ref?.Ref ? (resources[ref.Ref]?.Properties?.PathPart ?? "") : "";

    const open: string[] = [];
    for (const key of Object.keys(methods)) {
      const props = methods[key].Properties ?? {};
      const part = pathPart(props.ResourceId);
      const authType = props.AuthorizationType;
      if (part === "authorize") {
        // htsget-rs sends an unsigned GET; IAM auth here rejects every callout.
        expect(authType).toBe("NONE");
      } else {
        expect(authType).toBe("AWS_IAM");
      }
      if (authType === "NONE") {
        open.push(`${props.HttpMethod} ${part}`);
      }
      // No API-key shortcut around IAM either.
      expect(props.ApiKeyRequired).toBeFalsy();
    }

    // Exactly one open route, and it is the callout.
    expect(open).toEqual(["GET authorize"]);
  });

  it("is a PRIVATE endpoint bound to the interface endpoint", () => {
    // The callout carries no credential of any kind, so reachability is the only
    // control available for GET /authorize. PRIVATE means the hostname does not
    // resolve outside a VPC that has the endpoint.
    t.hasResourceProperties("AWS::ApiGateway::RestApi", {
      EndpointConfiguration: {
        Types: ["PRIVATE"],
        VpcEndpointIds: [Match.anyValue()],
      },
    });
    t.hasResourceProperties("AWS::EC2::VPCEndpoint", {
      VpcEndpointType: "Interface",
      PrivateDnsEnabled: true,
    });
  });

  it("denies every request that did not arrive through that endpoint", () => {
    const apis = t.findResources("AWS::ApiGateway::RestApi");
    const policy = Object.values(apis)[0].Properties.Policy;
    expect(policy).toBeDefined();

    const allow = policy.Statement.find((s: any) => s.Effect === "Allow");
    const deny = policy.Statement.find((s: any) => s.Effect === "Deny");

    // An allow alone would leave the door open to any future identity-policy
    // grant; the explicit deny is what makes the endpoint a hard boundary.
    expect(allow.Condition.StringEquals["aws:SourceVpce"]).toBeDefined();
    expect(deny).toBeDefined();
    expect(deny.Condition.StringNotEquals["aws:SourceVpce"]).toBeDefined();
    expect(deny.Action).toBe("execute-api:Invoke");
    expect(deny.Principal).toEqual({ AWS: "*" });
  });

  it("only opens 443 to the VPC's own address space", () => {
    t.hasResourceProperties("AWS::EC2::SecurityGroup", {
      SecurityGroupIngress: Match.arrayWith([
        Match.objectLike({ FromPort: 443, ToPort: 443, IpProtocol: "tcp" }),
      ]),
    });
    const sgs = t.findResources("AWS::EC2::SecurityGroup");
    for (const sg of Object.values(sgs)) {
      for (const rule of (sg as any).Properties.SecurityGroupIngress ?? []) {
        expect(rule.CidrIp).not.toBe("0.0.0.0/0");
      }
    }
  });

  it("grants operators execute-api:Invoke on /admin/* and nothing wider", () => {
    const withPolicy = synth({ provisionOperatorPolicy: true });
    const managed = withPolicy.findResources("AWS::IAM::ManagedPolicy");
    const doc = Object.values(managed)[0].Properties.PolicyDocument;
    const invoke = doc.Statement.find((s: any) => s.Sid === "InvokeAdminApi");
    expect(invoke).toBeDefined();
    expect(invoke.Action).toBe("execute-api:Invoke");
    const serialized = JSON.stringify(invoke.Resource);
    expect(serialized).toContain("/admin/*");
    expect(serialized).not.toContain("/*/*");
  });
});

describe("Lambda functions run inside the VPC", () => {
  const t = synth();

  it("attaches every function to VPC subnets with a security group", () => {
    const functions = t.findResources("AWS::Lambda::Function");
    expect(Object.keys(functions)).toHaveLength(2);
    for (const fn of Object.values(functions)) {
      const vpcConfig = (fn as any).Properties.VpcConfig;
      expect(vpcConfig).toBeDefined();
      expect(vpcConfig.SubnetIds.length).toBeGreaterThan(0);
      expect(vpcConfig.SecurityGroupIds.length).toBeGreaterThan(0);
    }
  });

  it("uses the VPC execution role variant on both function roles", () => {
    const roles = t.findResources("AWS::IAM::Role");
    const lambdaRoles = Object.values(roles).filter((r: any) =>
      JSON.stringify(r.Properties.AssumeRolePolicyDocument).includes(
        "lambda.amazonaws.com",
      ),
    );
    expect(lambdaRoles).toHaveLength(2);
    for (const role of lambdaRoles) {
      const arns = JSON.stringify((role as any).Properties.ManagedPolicyArns);
      expect(arns).toContain("AWSLambdaVPCAccessExecutionRole");
    }
  });

  it("gives the Lambda security group no ingress and only HTTPS egress", () => {
    const sgs = t.findResources("AWS::EC2::SecurityGroup");
    const lambdaSg: any = Object.values(sgs).find((sg: any) =>
      (sg.Properties.GroupDescription ?? "").includes("Lambda functions"),
    );
    expect(lambdaSg).toBeDefined();
    expect(lambdaSg.Properties.SecurityGroupIngress).toBeUndefined();
    for (const rule of lambdaSg.Properties.SecurityGroupEgress ?? []) {
      expect(rule.FromPort).toBe(443);
      expect(rule.ToPort).toBe(443);
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
