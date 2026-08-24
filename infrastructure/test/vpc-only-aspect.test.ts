import * as cdk from "aws-cdk-lib";
import { Aspects } from "aws-cdk-lib";
import * as apigw from "aws-cdk-lib/aws-apigateway";
import * as apigwv2 from "aws-cdk-lib/aws-apigatewayv2";
import * as ec2 from "aws-cdk-lib/aws-ec2";
import * as lambda from "aws-cdk-lib/aws-lambda";
import * as s3 from "aws-cdk-lib/aws-s3";
import {
  Annotations as AssertionAnnotations,
  Match,
  Template,
} from "aws-cdk-lib/assertions";
import { MockMartStack } from "../lib/mock-mart-stack";
import { VpcOnlyAspect } from "../lib/vpc-only-aspect";

function stackWithAspect(): cdk.Stack {
  const app = new cdk.App();
  const stack = new cdk.Stack(app, "AspectTest", {
    env: { account: "000000000000", region: "ap-southeast-2" },
  });
  Aspects.of(stack).add(new VpcOnlyAspect());
  return stack;
}

function synthErrors(stack: cdk.Stack): string {
  return AssertionAnnotations.fromStack(stack)
    .findError("*", Match.anyValue())
    .map((message) => String(message.entry.data))
    .join("\n");
}

const CODE = lambda.Code.fromInline("exports.handler = () => {};");

function noVpcFunction(stack: cdk.Stack, id = "Fn"): lambda.Function {
  return new lambda.Function(stack, id, {
    runtime: lambda.Runtime.NODEJS_20_X,
    handler: "index.handler",
    code: CODE,
  });
}

describe("VpcOnlyAspect", () => {
  it("rejects a Lambda function without a VpcConfig", () => {
    const stack = stackWithAspect();
    noVpcFunction(stack);
    expect(synthErrors(stack)).toContain("has no VpcConfig");
  });

  it("accepts a Lambda function attached to a VPC", () => {
    const stack = stackWithAspect();
    const vpc = new ec2.Vpc(stack, "Vpc", { natGateways: 0 });
    new lambda.Function(stack, "Fn", {
      runtime: lambda.Runtime.NODEJS_20_X,
      handler: "index.handler",
      code: CODE,
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_ISOLATED },
    });
    expect(synthErrors(stack)).toBe("");
  });

  it("rejects a non-private REST API", () => {
    const stack = stackWithAspect();
    const api = new apigw.RestApi(stack, "Api", {
      endpointConfiguration: { types: [apigw.EndpointType.REGIONAL] },
    });
    api.root.addMethod("GET");
    const errors = synthErrors(stack);
    expect(errors).toContain("is not a PRIVATE endpoint");
  });

  it("requires a resource policy even on a private REST API", () => {
    const stack = stackWithAspect();
    const api = new apigw.RestApi(stack, "Api", {
      endpointConfiguration: { types: [apigw.EndpointType.PRIVATE] },
    });
    api.root.addMethod("GET");
    expect(synthErrors(stack)).toContain("has no resource policy");
  });

  it("rejects any API Gateway v2 API", () => {
    const stack = stackWithAspect();
    new apigwv2.HttpApi(stack, "HttpApi");
    expect(synthErrors(stack)).toContain(
      "API Gateway v2 APIs cannot be made PRIVATE",
    );
  });

  it("rejects a Lambda function URL", () => {
    const stack = stackWithAspect();
    const vpc = new ec2.Vpc(stack, "Vpc", { natGateways: 0 });
    const fn = new lambda.Function(stack, "Fn", {
      runtime: lambda.Runtime.NODEJS_20_X,
      handler: "index.handler",
      code: CODE,
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_ISOLATED },
    });
    fn.addFunctionUrl({ authType: lambda.FunctionUrlAuthType.AWS_IAM });
    expect(synthErrors(stack)).toContain(
      "Lambda function URLs are public internet endpoints",
    );
  });

  it('rejects a Lambda permission granting invoke to principal "*"', () => {
    const stack = stackWithAspect();
    const vpc = new ec2.Vpc(stack, "Vpc", { natGateways: 0 });
    const fn = new lambda.Function(stack, "Fn", {
      runtime: lambda.Runtime.NODEJS_20_X,
      handler: "index.handler",
      code: CODE,
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_ISOLATED },
    });
    fn.addPermission("Public", {
      principal: new cdk.aws_iam.AnyPrincipal(),
      action: "lambda:InvokeFunction",
    });
    expect(synthErrors(stack)).toContain('grants invoke to principal "*"');
  });

  it("rejects a bucket that does not block all public access", () => {
    const stack = stackWithAspect();
    new s3.Bucket(stack, "Bucket");
    expect(synthErrors(stack)).toContain("does not block all public access");
  });

  it("keeps the mock stacks synthesisable", () => {
    const app = new cdk.App();
    const stack = new MockMartStack(app, "MockMart", {
      env: { account: "000000000000", region: "ap-southeast-2" },
      namePrefix: "data-releaser",
      databaseName: "mart_mock",
      workgroupName: "data-releaser-mock",
      dataBucketName: "data-share-test-000000000000-ap-southeast-2",
      cohortId: "DEMO",
      objects: [{ libraryId: "L2400001", key: "demo/L2400001/NA12878.bam" }],
    });
    Aspects.of(stack).add(new VpcOnlyAspect());
    expect(synthErrors(stack)).toBe("");
    const template = Template.fromStack(stack);

    const functions = template.findResources("AWS::Lambda::Function");
    expect(Object.keys(functions).length).toBeGreaterThan(0);
    for (const [logicalId] of Object.entries(functions)) {
      expect(logicalId).toMatch(
        /CDKBucketDeployment|CustomS3AutoDeleteObjects/,
      );
    }
  });
});
