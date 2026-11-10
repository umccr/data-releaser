import * as cdk from "aws-cdk-lib";
import { Template, Match } from "aws-cdk-lib/assertions";
import { DataReleaserStack, stateMachineExecutionArns } from "../lib/data-releaser-stack";

const RESEARCHER_A = "111111111111";
const RESEARCHER_B = "222222222222";

const COPY_STATE_MACHINE_ARN =
  "arn:aws:states:ap-southeast-2:333333333333:stateMachine:steps-s3-copy";
const COPY_WORKING_BUCKET = "steps-s3-copy-working";

function synth(overrides: Partial<Parameters<typeof buildStack>[0]> = {}) {
  return buildStack({
    provisionOperatorPolicy: false,
    researcherAccountIds: [RESEARCHER_A, RESEARCHER_B],
    ...overrides,
  });
}

function buildStack(opts: {
  provisionOperatorPolicy: boolean;
  researcherAccountIds: string[];
  copyStateMachineArn?: string;
  copyWorkingBucketName?: string;
}): Template {
  const app = new cdk.App();
  const stack = new DataReleaserStack(app, "TestStack", {
    env: { account: "000000000000", region: "ap-southeast-2" },
    namePrefix: "data-releaser",
    stageName: "prod",
    researcherAccountIds: opts.researcherAccountIds,
    cacheTtlCeiling: "1h",
    cacheDenyTtl: "60s",
    provisionOperatorPolicy: opts.provisionOperatorPolicy,
    apiVpcId: "vpc-12345",
    copyStateMachineArn: opts.copyStateMachineArn,
    copyWorkingBucketName: opts.copyWorkingBucketName,
  });
  return Template.fromStack(stack);
}

describe("share record table", () => {
  const t = synth();

  it("has partition key shareId and all three GSIs", () => {
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
        Match.objectLike({
          IndexName: "GSI3",
          KeySchema: [
            { AttributeName: "datasetUri", KeyType: "HASH" },
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

  it("configures no TTL", () => {
    const tables = t.findResources("AWS::DynamoDB::Table");
    for (const key of Object.keys(tables)) {
      const props = tables[key].Properties ?? {};
      if (props.TimeToLiveSpecification) {
        expect(props.TimeToLiveSpecification.Enabled).toBe(false);
      } else {
        expect(props.TimeToLiveSpecification).toBeUndefined();
      }
    }
  });
});

describe("API Gateway", () => {
  const t = synth();

  it("does NOT enable a response cache on the stage", () => {
    const stages = t.findResources("AWS::ApiGateway::Stage");
    expect(Object.keys(stages)).toHaveLength(1);
    const props = Object.values(stages)[0].Properties ?? {};

    expect(props.CacheClusterEnabled).toBeFalsy();
    expect(props.CacheClusterSize).toBeUndefined();
    for (const setting of props.MethodSettings ?? []) {
      expect(setting.CachingEnabled).toBeFalsy();
      expect(setting.CacheTtlInSeconds).toBeUndefined();
    }
  });

  it("does not require any request parameter on the callout", () => {
    const methods = t.findResources("AWS::ApiGateway::Method");
    for (const key of Object.keys(methods)) {
      const params = methods[key].Properties?.RequestParameters ?? {};
      for (const name of Object.keys(params)) {
        expect(name.startsWith("method.request.querystring.")).toBe(false);
      }

      const integration = methods[key].Properties?.Integration ?? {};
      expect(integration.CacheKeyParameters).toBeUndefined();
    }
  });

  it("requires SigV4 on every admin method", () => {
    const methods = t.findResources("AWS::ApiGateway::Method");
    const resources = t.findResources("AWS::ApiGateway::Resource");

    const pathPart = (ref: any): string =>
      ref?.Ref ? (resources[ref.Ref]?.Properties?.PathPart ?? "") : "";

    const open: string[] = [];
    for (const key of Object.keys(methods)) {
      const props = methods[key].Properties ?? {};
      const part = pathPart(props.ResourceId);
      const authType = props.AuthorizationType;
      if (part === "authorize") {
        expect(authType).toBe("NONE");
      } else {
        expect(authType).toBe("AWS_IAM");
      }
      if (authType === "NONE") {
        open.push(`${props.HttpMethod} ${part}`);
      }
      expect(props.ApiKeyRequired).toBeFalsy();
    }

    expect(open).toEqual(["GET authorize"]);
  });

  it("is a private endpoint bound to the interface", () => {
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

  it("grants operators execute-api:Invoke on /admin/*", () => {
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

describe("manifest bucket", () => {
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

  it("grants each researcher GetObject scoped to its own prefix under both mechanisms", () => {
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

    const policies = t.findResources("AWS::S3::BucketPolicy");
    const doc = Object.values(policies)[0].Properties.PolicyDocument;
    const getObjectStmts = doc.Statement.filter(
      (s: any) => s.Action === "s3:GetObject",
    );
    expect(getObjectStmts).toHaveLength(2);
    const asString = JSON.stringify(getObjectStmts);

    // Both prefixes, because a researcher may hold either kind of access or
    // both. A document written under a prefix the grant misses is readable by
    // nobody, and nothing reports that: the write is same-account and succeeds.
    for (const account of [RESEARCHER_A, RESEARCHER_B]) {
      expect(asString).toContain(`manifests/${account}/*`);
      expect(asString).toContain(`datasets/${account}/*`);
    }

    // Scoped to the account's own prefix and to reading, so one researcher
    // cannot reach another's document or write over their own.
    for (const s of getObjectStmts) {
      expect(s.Action).toBe("s3:GetObject");
      expect(s.Resource).toHaveLength(2);
    }
    expect(asString).not.toContain("manifests/*");
    expect(asString).not.toContain("datasets/*");
  });
});

describe("Execution roles are least-privilege", () => {
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
    expect(JSON.stringify(stmts)).not.toContain("dynamodb:*");
  });

  it("admin role reads/writes the table and queries GSI2 only", () => {
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

describe("Optional operator policy", () => {
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

describe("recording a finished dataset copy", () => {
  const t = synth({
    copyStateMachineArn: COPY_STATE_MACHINE_ARN,
    copyWorkingBucketName: COPY_WORKING_BUCKET,
  });

  /**
   * Logical id of the function that records a copy's outcome against the release
   * it was staging.
   */
  function handlerId(): string {
    const entries = Object.entries(t.findResources("AWS::Lambda::Function")).filter(
      ([, fn]) =>
        (fn as any).Properties.FunctionName === "data-releaser-copy-completion",
    );
    expect(entries).toHaveLength(1);
    return entries[0][0];
  }

  /**
   * The inline policy attached to that function's role, which is the whole of
   * what it may do.
   */
  function handlerPolicyDocument(): any {
    const roles = Object.entries(t.findResources("AWS::IAM::Role")).filter(
      ([, r]) =>
        (r as any).Properties.RoleName === "data-releaser-copy-completion-role",
    );
    expect(roles).toHaveLength(1);
    const [roleId] = roles[0];

    const policies = Object.values(t.findResources("AWS::IAM::Policy")).filter((p) =>
      ((p as any).Properties.Roles ?? []).some((r: any) => r.Ref === roleId),
    );
    expect(policies).toHaveLength(1);

    // Nothing else may be bolted onto the role either.
    const managed = Object.values(t.findResources("AWS::IAM::ManagedPolicy")).filter(
      (p) => ((p as any).Properties.Roles ?? []).some((r: any) => r.Ref === roleId),
    );
    expect(managed).toHaveLength(0);

    return (policies[0] as any).Properties.PolicyDocument;
  }

  it("deploys the handler inside the VPC alongside the other functions", () => {
    const functions = t.findResources("AWS::Lambda::Function");
    expect(Object.keys(functions)).toHaveLength(3);

    const handler = (functions as any)[handlerId()].Properties;
    expect(handler.Runtime).toBe("provided.al2023");
    expect(handler.Architectures).toEqual(["arm64"]);
    expect(handler.VpcConfig.SubnetIds.length).toBeGreaterThan(0);
    expect(handler.VpcConfig.SecurityGroupIds.length).toBeGreaterThan(0);
    expect(handler.Environment.Variables.SHARE_RECORDS_TABLE_NAME).toBeDefined();
  });

  it("routes copy status changes from that state machine to the handler", () => {
    t.resourceCountIs("AWS::Events::Rule", 1);
    t.hasResourceProperties("AWS::Events::Rule", {
      State: "ENABLED",
      EventPattern: {
        source: ["aws.states"],
        "detail-type": ["Step Functions Execution Status Change"],
        detail: { stateMachineArn: [COPY_STATE_MACHINE_ARN] },
      },
      Targets: Match.arrayWith([
        Match.objectLike({ Arn: { "Fn::GetAtt": [handlerId(), "Arn"] } }),
      ]),
    });
  });

  it("lets the rule invoke the handler and nothing else invoke it", () => {
    const permissions = Object.values(t.findResources("AWS::Lambda::Permission")).filter(
      (p) =>
        JSON.stringify((p as any).Properties.FunctionName).includes(handlerId()),
    );
    expect(permissions).toHaveLength(1);

    const props = (permissions[0] as any).Properties;
    expect(props.Principal).toBe("events.amazonaws.com");
    expect(props.Action).toBe("lambda:InvokeFunction");
    expect(JSON.stringify(props.SourceArn)).toContain("CopyCompletionRule");
  });

  it("matches every status change, leaving the handler to decide which end a copy", () => {
    const rules = t.findResources("AWS::Events::Rule");
    const pattern = Object.values(rules)[0].Properties.EventPattern;

    // Enumerating terminal statuses here as well would split that decision in
    // two, and the half living in the rule fails by dropping an event silently.
    expect(pattern.detail.status).toBeUndefined();
    expect(Object.keys(pattern.detail)).toEqual(["stateMachineArn"]);
  });

  it("reads one release and moves it on, without reaching for an index", () => {
    const record = handlerPolicyDocument().Statement.find(
      (s: any) => s.Sid === "RecordCopyOutcome",
    );

    expect(record.Action).toEqual(["dynamodb:GetItem", "dynamodb:UpdateItem"]);
    // A copy is named after the release it stages, so the record is addressed by
    // key: nothing is queried and nothing is scanned.
    const resource = JSON.stringify(record.Resource);
    expect(resource).not.toContain("index/");
    const document = JSON.stringify(handlerPolicyDocument());
    expect(document).not.toContain("dynamodb:Query");
    expect(document).not.toContain("dynamodb:Scan");
    expect(document).not.toContain("dynamodb:PutItem");
    expect(document).not.toContain("dynamodb:DeleteItem");
  });

  it("describes only executions of the copy state machine it was given", () => {
    const describe_ = handlerPolicyDocument().Statement.find(
      (s: any) => s.Sid === "DescribeCopyExecutions",
    );

    expect(describe_.Action).toBe("states:DescribeExecution");
    expect(describe_.Resource).toBe(
      "arn:aws:states:ap-southeast-2:333333333333:execution:steps-s3-copy:*",
    );
    expect(JSON.stringify(handlerPolicyDocument())).not.toContain(
      "states:StartExecution",
    );
  });

  it("reads the copy summary from the orchestration's own bucket only", () => {
    const read = handlerPolicyDocument().Statement.find(
      (s: any) => s.Sid === "ReadCopySummary",
    );

    expect(read.Action).toBe("s3:GetObject");
    expect(read.Resource).toBe(`arn:aws:s3:::${COPY_WORKING_BUCKET}/*`);
  });

  it("holds no grant on any dataset bucket or key", () => {
    const document = handlerPolicyDocument();
    expect(document.Statement).toHaveLength(3);

    const serialised = JSON.stringify(document);
    // The identical summary sits in the dataset's cache bucket, encrypted with
    // that dataset's key. Reading it there would mean this role holding decrypt
    // on every dataset key that will ever exist, extended by hand each time a
    // dataset is added — on the one component whose whole job is writing a few
    // numbers into a table. So: no KMS at all, and no S3 outside the working
    // bucket.
    expect(serialised).not.toContain("kms:");
    expect(serialised).not.toContain("-dataset-");

    for (const statement of document.Statement) {
      const resources = Array.isArray(statement.Resource)
        ? statement.Resource
        : [statement.Resource];
      for (const resource of resources) {
        expect(resource).not.toBe("*");
        if (typeof resource === "string" && resource.startsWith("arn:aws:s3:")) {
          expect(resource).toBe(`arn:aws:s3:::${COPY_WORKING_BUCKET}/*`);
        }
      }
      for (const action of Array.isArray(statement.Action)
        ? statement.Action
        : [statement.Action]) {
        expect(action).not.toContain(":*");
      }
    }
  });

  it("is absent when no copy orchestration is configured", () => {
    const bare = synth();

    expect(Object.keys(bare.findResources("AWS::Lambda::Function"))).toHaveLength(2);
    bare.resourceCountIs("AWS::Events::Rule", 0);
    expect(JSON.stringify(bare.toJSON())).not.toContain("copy-completion");
  });

  it("refuses a copy state machine without the bucket its summary lands in", () => {
    expect(() => synth({ copyStateMachineArn: COPY_STATE_MACHINE_ARN })).toThrow(
      /copyWorkingBucketName is required/,
    );
    expect(() => synth({ copyWorkingBucketName: COPY_WORKING_BUCKET })).toThrow(
      /copyStateMachineArn is required/,
    );
  });
});

describe("execution ARNs derived from a state machine", () => {
  it("keeps the state machine's own partition, region and account", () => {
    expect(stateMachineExecutionArns(COPY_STATE_MACHINE_ARN)).toBe(
      "arn:aws:states:ap-southeast-2:333333333333:execution:steps-s3-copy:*",
    );
    expect(
      stateMachineExecutionArns("arn:aws-cn:states:cn-north-1:999999999999:stateMachine:copy"),
    ).toBe("arn:aws-cn:states:cn-north-1:999999999999:execution:copy:*");
  });

  it("refuses an ARN that is not a state machine", () => {
    for (const arn of [
      "arn:aws:states:ap-southeast-2:333333333333:activity:steps-s3-copy",
      "arn:aws:states:ap-southeast-2:333333333333:execution:steps-s3-copy:run-1",
      "arn:aws:s3:::steps-s3-copy-working",
      "arn:aws:lambda:ap-southeast-2:333333333333:function:copy",
    ]) {
      expect(() => stateMachineExecutionArns(arn)).toThrow(/not a state machine ARN/);
    }
  });
});
