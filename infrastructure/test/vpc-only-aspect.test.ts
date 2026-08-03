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

/**
 * The VpcOnlyAspect is what makes "nothing public" a property of the build
 * rather than of review vigilance: these tests pin that every way of
 * configuring a public or VPC-less endpoint fails synth, and that the two CDK
 * custom-resource framework functions (deploy-time-only, CloudFormation-invoked)
 * remain the only exemption.
 */

/** A fresh stack with the aspect applied, plus a helper to synth it. */
function stackWithAspect(): cdk.Stack {
  const app = new cdk.App();
  const stack = new cdk.Stack(app, "AspectTest", {
    env: { account: "000000000000", region: "ap-southeast-2" },
  });
  Aspects.of(stack).add(new VpcOnlyAspect());
  return stack;
}

/**
 * All error annotations on the stack, joined for substring assertions. Error
 * annotations fail `cdk synth` / `cdk deploy` at the CLI; in unit tests they
 * are surfaced through the assertions module rather than by throwing.
 */
function synthErrors(stack: cdk.Stack): string {
  return AssertionAnnotations.fromStack(stack)
    .findError("*", Match.anyValue())
    .map((message) => String(message.entry.data))
    .join("\n");
}

/** Inline no-op function code for test fixtures. */
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

  it("rejects a non-PRIVATE REST API", () => {
    const stack = stackWithAspect();
    const api = new apigw.RestApi(stack, "Api", {
      endpointConfiguration: { types: [apigw.EndpointType.REGIONAL] },
    });
    api.root.addMethod("GET");
    const errors = synthErrors(stack);
    expect(errors).toContain("is not a PRIVATE endpoint");
  });

  it("requires a resource policy even on a PRIVATE REST API", () => {
    const stack = stackWithAspect();
    const api = new apigw.RestApi(stack, "Api", {
      endpointConfiguration: { types: [apigw.EndpointType.PRIVATE] },
    });
    api.root.addMethod("GET");
    expect(synthErrors(stack)).toContain("has no resource policy");
  });

  it("rejects any API Gateway v2 (HTTP) API outright", () => {
    const stack = stackWithAspect();
    new apigwv2.HttpApi(stack, "HttpApi");
    expect(synthErrors(stack)).toContain(
      "API Gateway v2 (HTTP/WebSocket) APIs cannot be made PRIVATE",
    );
  });

  it("rejects a Lambda function URL outright", () => {
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

  it("keeps the mock stacks synthesisable: only the CDK custom-resource providers are exempt", () => {
    // MockMartStack carries the two framework functions (BucketDeployment's
    // copier and the autoDeleteObjects cleaner). They are CloudFormation-invoked
    // deploy-time machinery with no network exposure, and must stay the ONLY
    // functions the aspect lets through without a VPC.
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

    // Every function present is one of the exempt framework providers.
    const functions = template.findResources("AWS::Lambda::Function");
    expect(Object.keys(functions).length).toBeGreaterThan(0);
    for (const [logicalId] of Object.entries(functions)) {
      expect(logicalId).toMatch(
        /CDKBucketDeployment|CustomS3AutoDeleteObjects/,
      );
    }
  });
});
