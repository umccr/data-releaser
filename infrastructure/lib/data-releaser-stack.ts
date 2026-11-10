import * as path from "path";
import {
  Arn,
  ArnFormat,
  Stack,
  StackProps,
  Duration,
  RemovalPolicy,
  CfnOutput,
  aws_dynamodb as dynamodb,
  aws_events as events,
  aws_events_targets as targets,
  aws_lambda as lambda,
  aws_apigateway as apigw,
  aws_s3 as s3,
  aws_iam as iam,
  aws_ec2 as ec2,
} from "aws-cdk-lib";
import * as goLambda from "@aws-cdk/aws-lambda-go-alpha";
import { Construct } from "constructs";

export interface DataReleaserStackProps extends StackProps {
  /**
   * Prefix applied to physical resource names.
   */
  readonly namePrefix: string;

  /**
   * API Gateway stage name.
   */
  readonly stageName: string;

  /**
   * Researcher AWS account IDs granted cross-account read on their own manifest prefix.
   */
  readonly researcherAccountIds: string[];

  /**
   * Ceiling on the `max-age` the authorization response may advertise.
   */
  readonly cacheTtlCeiling: string;

  /**
   * Negative-cache max-age as a Go duration string.
   */
  readonly cacheDenyTtl: string;

  /**
   * When true, provision the operator IAM policy for running the CLI.
   */
  readonly provisionOperatorPolicy: boolean;

  /**
   * VPC id hosting the `execute-api` interface endpoint through which the
   * authorization callout reaches this API.
   *
   * This must be the same VPC the htsget-rs Lambda is attached as a private API is
   * reachable only through an interface endpoint in a VPC the caller has ENIs in.
   */
  readonly apiVpcId: string;

  /**
   * ARN of the state machine that copies datasets into their cache buckets.
   *
   * The copy orchestration is deployed separately and consumed here, so this is
   * configuration rather than something the stack creates. Setting it deploys the
   * function that records a finished copy against the release it was staging,
   * together with the rule delivering those status changes to it. Leaving it out
   * deploys neither, which is what a deployment releasing only through htsget
   * wants.
   *
   * An orchestration installed in another account needs a rule there forwarding
   * its execution events onto this account's bus; the rule here matches only what
   * already reaches it.
   */
  readonly copyStateMachineArn?: string;

  /**
   * Bucket the copy orchestration works in, and where it retains its summary of
   * each run.
   *
   * Required alongside `copyStateMachineArn`: that summary is where the object
   * count and byte total recorded against a release come from.
   */
  readonly copyWorkingBucketName?: string;
}

/**
 * ARN pattern covering every execution of one state machine, which is the
 * resource form `states:DescribeExecution` is granted on.
 *
 * Built from the state machine's own ARN rather than from this stack's account
 * and region, because the copy orchestration is deployed separately and need not
 * sit in either.
 */
export function stateMachineExecutionArns(stateMachineArn: string): string {
  const parts = Arn.split(stateMachineArn, ArnFormat.COLON_RESOURCE_NAME);
  if (parts.service !== "states" || parts.resource !== "stateMachine" || !parts.resourceName) {
    throw new Error(
      `${JSON.stringify(stateMachineArn)} is not a state machine ARN, so the executions to ` +
        "grant access to cannot be derived from it",
    );
  }

  return Arn.format({
    partition: parts.partition,
    service: parts.service,
    region: parts.region,
    account: parts.account,
    resource: "execution",
    resourceName: `${parts.resourceName}:*`,
    arnFormat: ArnFormat.COLON_RESOURCE_NAME,
  });
}

const GSI1_NAME = "GSI1";
const GSI2_NAME = "GSI2";
const GSI3_NAME = "GSI3";

/**
 * Manifest object key prefixes, one per release mechanism.
 *
 * Both mechanisms deliver a researcher's document into this one bucket and
 * differ only in the prefix, so that a researcher holding both kinds of access
 * does not have one document overwrite the other. Each has to match the key
 * template the CLI renders for that mechanism, or the grant written here covers
 * a location nothing is ever written to.
 */
const MANIFEST_PREFIX = "manifests";
const DATASET_MANIFEST_PREFIX = "datasets";

/**
 * DataReleaserStack provisions, into the data control account, every AWS
 * resource the authorization needs.
 */
export class DataReleaserStack extends Stack {
  constructor(scope: Construct, id: string, props: DataReleaserStackProps) {
    super(scope, id, props);

    const prefix = props.namePrefix;

    const vpc = ec2.Vpc.fromLookup(this, "ApiVpc", { vpcId: props.apiVpcId });

    const lambdaSecurityGroup = new ec2.SecurityGroup(this, "LambdaSg", {
      vpc,
      description: `${prefix} Lambda functions, HTTPS egress only.`,
      allowAllOutbound: false,
    });
    lambdaSecurityGroup.addEgressRule(
      ec2.Peer.anyIpv4(),
      ec2.Port.tcp(443),
      "HTTPS to AWS service endpoints",
    );

    // Both functions sit in the private-with-egress subnets.
    const lambdaVpcConfig = {
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
      securityGroups: [lambdaSecurityGroup],
    };

    const table = new dynamodb.Table(this, "ShareRecords", {
      tableName: `${prefix}-ShareRecords`,
      partitionKey: { name: "shareId", type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      pointInTimeRecovery: true,
      removalPolicy: RemovalPolicy.RETAIN,
    });

    // GSI1, i.e. authorization callout lookup by targetVpcArn.
    table.addGlobalSecondaryIndex({
      indexName: GSI1_NAME,
      partitionKey: { name: "targetVpcArn", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.INCLUDE,
      nonKeyAttributes: ["s3Prefixes", "expiresAt", "revokedAt", "createdAt"],
    });

    // GSI2, i.e. per-researcher listing and idempotency.
    table.addGlobalSecondaryIndex({
      indexName: GSI2_NAME,
      partitionKey: { name: "researcherAccountId", type: dynamodb.AttributeType.STRING },
      sortKey: { name: "createdAt", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });

    // GSI3, i.e. every release of one dataset, which staging and purge both key on.
    // Only S3 dataset records carry a datasetUri, so the index is inherently limited to
    // them and needs no mechanism filter on the query.
    table.addGlobalSecondaryIndex({
      indexName: GSI3_NAME,
      partitionKey: { name: "datasetUri", type: dynamodb.AttributeType.STRING },
      sortKey: { name: "createdAt", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });

    const gsi1Arn = `${table.tableArn}/index/${GSI1_NAME}`;
    const gsi2Arn = `${table.tableArn}/index/${GSI2_NAME}`;
    const gsi3Arn = `${table.tableArn}/index/${GSI3_NAME}`;

    const goModuleDir = path.join(__dirname, "..", "..", "go.mod");
    const goBundling: goLambda.BundlingOptions = {
      goBuildFlags: ['-trimpath -ldflags "-s -w"'],
    };

    // Authorization callout role: read-only.
    const authRole = new iam.Role(this, "AuthorizationFunctionRole", {
      roleName: `${prefix}-authorization-role`,
      assumedBy: new iam.ServicePrincipal("lambda.amazonaws.com"),
      managedPolicies: [
        iam.ManagedPolicy.fromAwsManagedPolicyName(
          "service-role/AWSLambdaVPCAccessExecutionRole",
        ),
      ],
    });
    // Needs GSI1 only.
    authRole.addToPolicy(
      new iam.PolicyStatement({
        sid: "QueryGsi1Only",
        actions: ["dynamodb:Query"],
        resources: [gsi1Arn],
      }),
    );

    const authFn = new goLambda.GoFunction(this, "AuthorizationFunction", {
      functionName: `${prefix}-authorization`,
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      entry: path.join(__dirname, "..", "..", "functions", "authorization"),
      moduleDir: goModuleDir,
      bundling: goBundling,
      role: authRole,
      memorySize: 128,
      timeout: Duration.seconds(10),
      ...lambdaVpcConfig,
      environment: {
        SHARE_RECORDS_TABLE_NAME: table.tableName,
        CACHE_TTL_CEILING: props.cacheTtlCeiling,
        CACHE_DENY_TTL: props.cacheDenyTtl,
      },
    });

    // Admin role.
    const adminRole = new iam.Role(this, "AdminFunctionRole", {
      roleName: `${prefix}-admin-role`,
      assumedBy: new iam.ServicePrincipal("lambda.amazonaws.com"),
      managedPolicies: [
        iam.ManagedPolicy.fromAwsManagedPolicyName(
          "service-role/AWSLambdaVPCAccessExecutionRole",
        ),
      ],
    });
    adminRole.addToPolicy(
      new iam.PolicyStatement({
        sid: "ReadWriteTable",
        actions: [
          "dynamodb:GetItem",
          "dynamodb:PutItem",
          "dynamodb:UpdateItem",
          "dynamodb:Query",
          "dynamodb:Scan",
        ],
        resources: [table.tableArn],
      }),
    );
    adminRole.addToPolicy(
      new iam.PolicyStatement({
        sid: "QueryGsi2Only",
        actions: ["dynamodb:Query"],
        resources: [gsi2Arn],
      }),
    );

    const adminFn = new goLambda.GoFunction(this, "AdminFunction", {
      functionName: `${prefix}-admin`,
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      entry: path.join(__dirname, "..", "..", "functions", "admin"),
      moduleDir: goModuleDir,
      bundling: goBundling,
      role: adminRole,
      memorySize: 128,
      timeout: Duration.seconds(10),
      ...lambdaVpcConfig,
      environment: {
        SHARE_RECORDS_TABLE_NAME: table.tableName,
      },
    });

    // A dataset copy runs for hours or days, so nothing waits on it. The copy
    // orchestration announces its own terminal state, and this function records
    // that against the release the copy was staging — moving it to staged, where
    // it can be granted to a researcher, or to failed, where it cannot.
    const copyStateMachineArn = props.copyStateMachineArn;
    const copyWorkingBucketName = props.copyWorkingBucketName;
    if (copyStateMachineArn && !copyWorkingBucketName) {
      throw new Error(
        "copyWorkingBucketName is required alongside copyStateMachineArn: a finished copy " +
          "retains its account of what it moved in the orchestration's working bucket, and that " +
          "is where a release's object count and byte total come from",
      );
    }
    if (copyWorkingBucketName && !copyStateMachineArn) {
      throw new Error(
        "copyStateMachineArn is required alongside copyWorkingBucketName: without it nothing " +
          "listens for a finished copy, and a dataset whose copy has completed would sit in the " +
          "staging state indefinitely, never becoming grantable",
      );
    }

    if (copyStateMachineArn && copyWorkingBucketName) {
      const copyCompletionRole = new iam.Role(this, "CopyCompletionFunctionRole", {
        roleName: `${prefix}-copy-completion-role`,
        assumedBy: new iam.ServicePrincipal("lambda.amazonaws.com"),
        managedPolicies: [
          iam.ManagedPolicy.fromAwsManagedPolicyName(
            "service-role/AWSLambdaVPCAccessExecutionRole",
          ),
        ],
      });

      // Read one release and move it on. A copy is named after the release it
      // stages, so the record is addressed by key: no index is queried and
      // nothing is scanned.
      copyCompletionRole.addToPolicy(
        new iam.PolicyStatement({
          sid: "RecordCopyOutcome",
          actions: ["dynamodb:GetItem", "dynamodb:UpdateItem"],
          resources: [table.tableArn],
        }),
      );

      // Asking the orchestration what an execution returned covers the events
      // whose payload the bus dropped for exceeding its size limit.
      copyCompletionRole.addToPolicy(
        new iam.PolicyStatement({
          sid: "DescribeCopyExecutions",
          actions: ["states:DescribeExecution"],
          resources: [stateMachineExecutionArns(copyStateMachineArn)],
        }),
      );

      // The copy's summary of what it moved, read from the orchestration's own
      // bucket. The identical copy in the dataset's cache bucket is deliberately
      // not read: that one is encrypted with the dataset's key, so reading it
      // would mean this role holding decrypt on every dataset key that will ever
      // exist — extended by hand each time a dataset is added, on the one
      // component whose whole job is to write a few numbers into a table. Hence
      // no grant here on any dataset bucket or key.
      copyCompletionRole.addToPolicy(
        new iam.PolicyStatement({
          sid: "ReadCopySummary",
          actions: ["s3:GetObject"],
          resources: [`arn:aws:s3:::${copyWorkingBucketName}/*`],
        }),
      );

      const copyCompletionFn = new goLambda.GoFunction(this, "CopyCompletionFunction", {
        functionName: `${prefix}-copy-completion`,
        runtime: lambda.Runtime.PROVIDED_AL2023,
        architecture: lambda.Architecture.ARM_64,
        entry: path.join(__dirname, "..", "..", "functions", "copycompletion"),
        moduleDir: goModuleDir,
        bundling: goBundling,
        role: copyCompletionRole,
        // Larger than the other two functions because the summary carries a row
        // per object and is totalled in one pass, so a dataset of many objects
        // makes for a sizeable document.
        memorySize: 512,
        timeout: Duration.minutes(1),
        ...lambdaVpcConfig,
        environment: {
          SHARE_RECORDS_TABLE_NAME: table.tableName,
        },
      });

      // Every status change on the copy state machine, not only the terminal
      // ones. Which statuses end a copy is the function's to know; enumerating
      // them here as well would split that decision across two places, and the
      // half living in the rule fails by dropping an event silently. A running
      // execution costs one invocation that records nothing, and a cost estimate
      // shares the state machine but is named after a dataset rather than a
      // release, so it matches no record and is reported as staging nothing.
      const copyCompletionRule = new events.Rule(this, "CopyCompletionRule", {
        ruleName: `${prefix}-copy-completion`,
        description:
          "Copy execution status changes, which is how a staging dataset learns its copy finished.",
        eventPattern: {
          source: ["aws.states"],
          detailType: ["Step Functions Execution Status Change"],
          detail: { stateMachineArn: [copyStateMachineArn] },
        },
      });
      copyCompletionRule.addTarget(new targets.LambdaFunction(copyCompletionFn));
    }

    const endpointSecurityGroup = new ec2.SecurityGroup(this, "ApiVpcEndpointSg", {
      vpc,
      description: `HTTPS to the ${prefix} authorization API interface endpoint.`,
      allowAllOutbound: false,
    });
    endpointSecurityGroup.addIngressRule(
      ec2.Peer.ipv4(vpc.vpcCidrBlock),
      ec2.Port.tcp(443),
      "htsget-rs authorization callout",
    );

    const vpcEndpoint = new ec2.InterfaceVpcEndpoint(this, "ApiVpcEndpoint", {
      vpc,
      service: ec2.InterfaceVpcEndpointAwsService.APIGATEWAY,
      privateDnsEnabled: true,
      securityGroups: [endpointSecurityGroup],
      subnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
    });

    // A private API is deny by default.
    const apiPolicy = new iam.PolicyDocument({
      statements: [
        new iam.PolicyStatement({
          sid: "AllowFromAuthorizationVpcEndpoint",
          effect: iam.Effect.ALLOW,
          principals: [new iam.AnyPrincipal()],
          actions: ["execute-api:Invoke"],
          resources: ["execute-api:/*"],
          conditions: {
            StringEquals: { "aws:SourceVpce": vpcEndpoint.vpcEndpointId },
          },
        }),
        new iam.PolicyStatement({
          sid: "DenyFromAnywhereElse",
          effect: iam.Effect.DENY,
          principals: [new iam.AnyPrincipal()],
          actions: ["execute-api:Invoke"],
          resources: ["execute-api:/*"],
          conditions: {
            StringNotEquals: { "aws:SourceVpce": vpcEndpoint.vpcEndpointId },
          },
        }),
      ],
    });

    const api = new apigw.RestApi(this, "AuthorizationApi", {
      restApiName: `${prefix}-authorization-api`,
      description: "Authorization Service REST API (callout + admin).",
      deployOptions: {
        stageName: props.stageName,
      },
      endpointConfiguration: {
        types: [apigw.EndpointType.PRIVATE],
        vpcEndpoints: [vpcEndpoint],
      },
      policy: apiPolicy,
    });

    // Authorization callout: GET /authorize, which is the htsget-rs callout.
    const authorize = api.root.addResource("authorize");
    authorize.addMethod("GET", new apigw.LambdaIntegration(authFn, { proxy: true }));

    // Admin interface called by CLI.
    const adminIntegration = new apigw.LambdaIntegration(adminFn, { proxy: true });
    const adminAuth: apigw.MethodOptions = {
      authorizationType: apigw.AuthorizationType.IAM,
    };
    const admin = api.root.addResource("admin");
    const shares = admin.addResource("shares");
    shares.addMethod("POST", adminIntegration, adminAuth);
    shares.addMethod("GET", adminIntegration, adminAuth);
    shares.addResource("revoke").addMethod("POST", adminIntegration, adminAuth);
    shares.addResource("extend").addMethod("POST", adminIntegration, adminAuth);

    const manifestBucket = new s3.Bucket(this, "ManifestBucket", {
      bucketName: `${prefix}-manifests-${this.account}`,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      versioned: true,
      removalPolicy: RemovalPolicy.RETAIN,
    });

    // One bucket-policy statement per onboarded researcher account, covering
    // both mechanisms' prefixes.
    //
    // A researcher may hold either kind of access or both, and which they hold
    // changes over the life of the account without this stack being redeployed.
    // Granting only the prefix they happen to use today would leave a manifest
    // that is written successfully and cannot be read, and nothing would report
    // it: the write is same-account and succeeds, and the document is
    // descriptive only, so the researcher keeps working access and simply
    // cannot discover where the data is.
    //
    // Reading a prefix that holds no document is not a disclosure. The account
    // is scoped to its own prefix under both, so the widest this reaches is an
    // account learning that it has no manifest of one kind, which it already
    // knows.
    for (const accountId of props.researcherAccountIds) {
      manifestBucket.addToResourcePolicy(
        new iam.PolicyStatement({
          sid: `ResearcherRead${accountId}`,
          effect: iam.Effect.ALLOW,
          principals: [new iam.AccountPrincipal(accountId)],
          actions: ["s3:GetObject"],
          resources: [
            manifestBucket.arnForObjects(`${MANIFEST_PREFIX}/${accountId}/*`),
            manifestBucket.arnForObjects(
              `${DATASET_MANIFEST_PREFIX}/${accountId}/*`,
            ),
          ],
        }),
      );
    }

    // Operator policy
    if (props.provisionOperatorPolicy) {
      const operatorPolicy = new iam.ManagedPolicy(this, "OperatorPolicy", {
        managedPolicyName: `${prefix}-operator`,
        description:
          "Data Releaser operator permissions.",
        statements: [
          new iam.PolicyStatement({
            sid: "AthenaQuery",
            actions: [
              "athena:StartQueryExecution",
              "athena:GetQueryExecution",
              "athena:GetQueryResults",
              "athena:StopQueryExecution",
              "glue:GetDatabase",
              "glue:GetTable",
              "glue:GetTables",
              "glue:GetPartitions",
              "lakeformation:GetDataAccess",
            ],
            resources: ["*"],
          }),
          new iam.PolicyStatement({
            sid: "ShareRecords",
            actions: [
              "dynamodb:GetItem",
              "dynamodb:PutItem",
              "dynamodb:UpdateItem",
              "dynamodb:Query",
              "dynamodb:Scan",
            ],
            resources: [table.tableArn, gsi1Arn, gsi2Arn, gsi3Arn],
          }),
          new iam.PolicyStatement({
            sid: "ReadApiStage",
            actions: ["apigateway:GET"],
            resources: [
              `arn:aws:apigateway:${this.region}::/restapis/${api.restApiId}/stages/${props.stageName}`,
            ],
          }),
          new iam.PolicyStatement({
            sid: "InvokeAdminApi",
            actions: ["execute-api:Invoke"],
            resources: [api.arnForExecuteApi("*", "/admin/*", props.stageName)],
          }),
          new iam.PolicyStatement({
            sid: "LatticeAndRam",
            actions: [
              "vpc-lattice:GetServiceNetwork",
              "vpc-lattice:ListServiceNetworkVpcAssociations",
              "vpc-lattice:ListServiceNetworks",
              "ram:GetResourceShareAssociations",
              "ram:ListResources",
              "ram:GetResourceShares",
            ],
            resources: ["*"],
          }),
          new iam.PolicyStatement({
            sid: "ManifestWrite",
            actions: ["s3:PutObject", "s3:DeleteObject", "s3:GetObject", "s3:ListBucket"],
            resources: [manifestBucket.bucketArn, manifestBucket.arnForObjects("*")],
          }),
        ],
      });

      new CfnOutput(this, "OperatorPolicyArn", {
        value: operatorPolicy.managedPolicyArn,
        description: "ARN of the optional operator IAM policy.",
      });
    }

    new CfnOutput(this, "ApiGatewayRestApiId", {
      value: api.restApiId,
      description: "CLI config: endpoints.apiGatewayRestApiId",
      exportName: `${prefix}-apiGatewayRestApiId`,
    });
    new CfnOutput(this, "ApiGatewayStageName", {
      value: props.stageName,
      description: "CLI config: endpoints.apiGatewayStageName",
      exportName: `${prefix}-apiGatewayStageName`,
    });
    new CfnOutput(this, "ManifestBucketName", {
      value: manifestBucket.bucketName,
      description: "CLI config: manifest.bucket",
      exportName: `${prefix}-manifestBucket`,
    });
    new CfnOutput(this, "AuthorizationCalloutUrl", {
      value: `${api.url}authorize`,
      description:
        "CLI/htsget config: endpoints.authorization",
    });
    new CfnOutput(this, "AuthorizationApiVpcEndpointId", {
      value: vpcEndpoint.vpcEndpointId,
      description:
        "The only route to the authorization API. Callers must have ENIs in this VPC.",
    });
  }
}
