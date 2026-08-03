import { Annotations, IAspect, Stack } from "aws-cdk-lib";
import { CfnRestApi } from "aws-cdk-lib/aws-apigateway";
import { CfnApi } from "aws-cdk-lib/aws-apigatewayv2";
import { CfnFunction, CfnPermission, CfnUrl } from "aws-cdk-lib/aws-lambda";
import { CfnBucket } from "aws-cdk-lib/aws-s3";
import { IConstruct } from "constructs";

/**
 * Construct-path patterns for the only Lambda functions allowed to exist
 * without a VPC configuration: the CDK-vended CloudFormation custom-resource
 * providers (`BucketDeployment`'s copier and the `autoDeleteObjects` cleaner).
 *
 * These are deploy-time machinery, not service endpoints: they are invoked
 * only by CloudFormation during a stack operation (no resource policy opens
 * them to anything else — the aspect's CfnPermission check would catch one),
 * they expose no URL, no API and no Lattice target, and they exist solely so
 * `cdk deploy` can copy fixture objects into (and later empty) the disposable
 * mock buckets.
 * Attaching them to the shared landing-zone VPC would gain nothing — they are
 * unreachable from any network already — and would break them unless that VPC
 * happened to provide S3 connectivity for them.
 *
 * Anything not matching these exact framework paths must have a VPC.
 */
const VPC_EXEMPT_FUNCTION_PATHS: RegExp[] = [
  /\/Custom::CDKBucketDeployment[^/]*\//,
  /\/Custom::S3AutoDeleteObjectsCustomResourceProvider\//,
];

/**
 * A CDK Aspect that makes a PUBLIC deployment unsynthesisable.
 *
 * The Data Releaser handles controlled-access genomic data, and its only
 * intended entry points are private and VPC-bound (Req: the researcher reaches
 * htsget-rs over VPC Lattice; htsget-rs reaches the authorization callout over
 * a private API Gateway endpoint). This aspect turns that intent into a
 * synth-time invariant rather than a convention:
 *
 *  - every Lambda function MUST have a VpcConfig (no ENI-less functions), with
 *    the single documented exception of CDK's own CloudFormation
 *    custom-resource providers (see {@link VPC_EXEMPT_FUNCTION_PATHS});
 *  - every API Gateway REST API MUST be endpointConfiguration PRIVATE and MUST
 *    carry a resource policy (a private API without a policy allows nothing,
 *    but requiring the policy keeps the deny-by-default explicit);
 *  - API Gateway v2 (HTTP/WebSocket) APIs are forbidden outright — they have
 *    no private endpoint type, so any instance is internet-facing;
 *  - Lambda function URLs are forbidden outright — they are public internet
 *    endpoints regardless of auth type;
 *  - no Lambda resource policy may grant invoke to principal `*`;
 *  - every S3 bucket MUST block all four public-access vectors.
 *
 * Violations are reported with `Annotations.addError`, which fails `cdk synth`
 * and `cdk deploy` before anything reaches AWS (unit tests assert on them via
 * `Annotations.fromStack` from the assertions module). There is
 * deliberately no opt-out flag: making an endpoint public again requires
 * editing this aspect, which is a reviewable code change, not a context value.
 */
export class VpcOnlyAspect implements IAspect {
  public visit(node: IConstruct): void {
    if (node instanceof CfnFunction) {
      this.visitFunction(node);
    } else if (node instanceof CfnUrl) {
      Annotations.of(node).addError(
        "Lambda function URLs are public internet endpoints and are forbidden " +
          "in the Data Releaser infrastructure (VpcOnlyAspect).",
      );
    } else if (node instanceof CfnPermission) {
      this.visitPermission(node);
    } else if (node instanceof CfnRestApi) {
      this.visitRestApi(node);
    } else if (node instanceof CfnApi) {
      Annotations.of(node).addError(
        "API Gateway v2 (HTTP/WebSocket) APIs cannot be made PRIVATE and are " +
          "forbidden in the Data Releaser infrastructure; use a PRIVATE REST " +
          "API behind an interface endpoint instead (VpcOnlyAspect).",
      );
    } else if (node instanceof CfnBucket) {
      this.visitBucket(node);
    }
  }

  private visitFunction(fn: CfnFunction): void {
    if (VPC_EXEMPT_FUNCTION_PATHS.some((p) => p.test(fn.node.path))) {
      return;
    }

    const vpcConfig = Stack.of(fn).resolve(fn.vpcConfig) as
      | { subnetIds?: unknown[] }
      | undefined;
    if (
      vpcConfig === undefined ||
      !Array.isArray(vpcConfig.subnetIds) ||
      vpcConfig.subnetIds.length === 0
    ) {
      Annotations.of(fn).addError(
        `Lambda function ${fn.node.path} has no VpcConfig. Every function in ` +
          "the Data Releaser infrastructure must run inside the VPC " +
          "(VpcOnlyAspect); attach it with vpc/vpcSubnets/securityGroups.",
      );
    }
  }

  private visitPermission(permission: CfnPermission): void {
    const principal = Stack.of(permission).resolve(permission.principal) as
      | string
      | undefined;
    if (principal === "*") {
      Annotations.of(permission).addError(
        `Lambda permission ${permission.node.path} grants invoke to principal ` +
          '"*", which would make the function publicly invokable ' +
          "(VpcOnlyAspect).",
      );
    }
  }

  private visitRestApi(api: CfnRestApi): void {
    const endpointConfiguration = Stack.of(api).resolve(
      api.endpointConfiguration,
    ) as { types?: string[] } | undefined;
    const types = endpointConfiguration?.types ?? [];
    if (types.length !== 1 || types[0] !== "PRIVATE") {
      Annotations.of(api).addError(
        `REST API ${api.node.path} is not a PRIVATE endpoint ` +
          `(endpointConfiguration.types = ${JSON.stringify(types)}). Every API ` +
          "Gateway in the Data Releaser infrastructure must be PRIVATE and " +
          "reachable only through its VPC interface endpoint (VpcOnlyAspect).",
      );
    }

    if (Stack.of(api).resolve(api.policy) === undefined) {
      Annotations.of(api).addError(
        `REST API ${api.node.path} has no resource policy. A private API must ` +
          "pin execute-api:Invoke to its own VPC endpoint with an explicit " +
          "deny for every other path (VpcOnlyAspect).",
      );
    }
  }

  private visitBucket(bucket: CfnBucket): void {
    const config = Stack.of(bucket).resolve(
      bucket.publicAccessBlockConfiguration,
    ) as
      | {
          blockPublicAcls?: boolean;
          blockPublicPolicy?: boolean;
          ignorePublicAcls?: boolean;
          restrictPublicBuckets?: boolean;
        }
      | undefined;
    const blocked =
      config !== undefined &&
      config.blockPublicAcls === true &&
      config.blockPublicPolicy === true &&
      config.ignorePublicAcls === true &&
      config.restrictPublicBuckets === true;
    if (!blocked) {
      Annotations.of(bucket).addError(
        `S3 bucket ${bucket.node.path} does not block all public access. Every ` +
          "bucket in the Data Releaser infrastructure must set " +
          "blockPublicAccess: BLOCK_ALL (VpcOnlyAspect).",
      );
    }
  }
}
