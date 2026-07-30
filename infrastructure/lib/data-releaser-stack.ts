import * as path from "path";
import {
  Stack,
  StackProps,
  Duration,
  RemovalPolicy,
  CfnOutput,
  aws_dynamodb as dynamodb,
  aws_lambda as lambda,
  aws_apigateway as apigw,
  aws_s3 as s3,
  aws_kms as kms,
  aws_iam as iam,
} from "aws-cdk-lib";
import { Construct } from "constructs";

/** Manifest_Bucket server-side encryption mode (Req 16.5). */
export type EncryptionMode = "s3-managed" | "kms";

export interface DataReleaserStackProps extends StackProps {
  /**
   * Prefix applied to physical resource names so multiple environments can
   * coexist and names are recognisable (Req 16.10). e.g. "data-releaser".
   */
  readonly namePrefix: string;

  /** API Gateway stage name; also emitted as the apiGatewayStageName output. */
  readonly stageName: string;

  /**
   * Researcher AWS account IDs granted cross-account read on their own manifest
   * prefix. May be empty. Onboarding a researcher is adding an ID here — a
   * configuration change, not a code change (Req 16.5).
   */
  readonly researcherAccountIds: string[];

  /**
   * Ceiling on the `max-age` the authorization response may advertise, as a Go
   * duration string (e.g. "1h"). Passed verbatim to the authorization Lambda as
   * CACHE_TTL_CEILING (it parses Go durations) and used there to cap the
   * computed Cache-Control max-age. Nothing in this stack caches responses.
   */
  readonly cacheTtlCeiling: string;

  /** Negative-cache max-age as a Go duration string (e.g. "60s") (Req 13.2). */
  readonly cacheDenyTtl: string;

  /** Manifest_Bucket encryption mode (Req 16.5). */
  readonly encryption: EncryptionMode;

  /**
   * When true, provision the operator IAM policy of Req 11.2 for humans who run
   * the CLI. This does NOT change Req 11.3: the CLI itself still creates no IAM
   * at runtime. Toggleable so environments that manage operator permissions
   * elsewhere can leave it off (Req 16.9).
   */
  readonly provisionOperatorPolicy: boolean;
}

/** DynamoDB GSI names — must match the constants the Go handlers query against. */
const GSI1_NAME = "GSI1"; // targetVpcArn — authorization callout lookup (Req 4.2)
const GSI2_NAME = "GSI2"; // researcherAccountId + createdAt — list / idempotency

/** Query-string parameter carrying the requester identity (source VPC ARN). */
const SOURCE_VPC_ARN_PARAM = "sourceVpcArn";

/** Manifest object key prefix (Req 7.9 / 16.5): manifests/<account-id>/... */
const MANIFEST_PREFIX = "manifests";

/**
 * DataReleaserStack provisions, into the Data_Control_Account, every AWS
 * resource the Authorization_Service needs (Req 16.1): the Share_Record table,
 * the two Go Lambdas, the REST API fronting them, the Manifest_Bucket with its
 * cross-account read policy, tightly-scoped execution roles, stack outputs, and
 * (optionally) the operator IAM policy.
 *
 * It deliberately provisions NOTHING outside the Data Control account — no
 * researcher-side resource, no Lake Formation / RAM sharing, no htsget-rs
 * (Req 16.8).
 */
export class DataReleaserStack extends Stack {
  constructor(scope: Construct, id: string, props: DataReleaserStackProps) {
    super(scope, id, props);

    const prefix = props.namePrefix;

    // ---------------------------------------------------------------------
    // Share_Record DynamoDB table (Req 16.2, 10.2, 8.2)
    // ---------------------------------------------------------------------
    const table = new dynamodb.Table(this, "ShareRecords", {
      tableName: `${prefix}-ShareRecords`,
      partitionKey: { name: "shareId", type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      // Deliberately NO timeToLiveAttribute: expired and revoked Share_Records
      // must remain visible to `list` and audit; effective status is derived at
      // read time, records are never auto-deleted (Req 8.2).
      pointInTimeRecovery: true,
      removalPolicy: RemovalPolicy.RETAIN, // audit data — never destroy on stack delete
    });

    // GSI1: authorization callout lookup by targetVpcArn (Req 4.2).
    // Project ONLY the attributes the callout handler actually reads back
    // (internal/auth/callout.go -> unmarshalShareRecord): the S3 prefixes it
    // returns as locations, and expiresAt/revokedAt/createdAt which the
    // effective-status computation and unmarshalling require. shareId,
    // researcherAccountId and libraryIds are never read on this path, so they
    // are excluded to keep the index minimal (Req 16.7 spirit; task 22.2).
    table.addGlobalSecondaryIndex({
      indexName: GSI1_NAME,
      partitionKey: { name: "targetVpcArn", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.INCLUDE,
      nonKeyAttributes: ["s3Prefixes", "expiresAt", "revokedAt", "createdAt"],
    });

    // GSI2: per-researcher listing and idempotency (Req 8, 12). The admin
    // handler reads full records back from this index (create idempotency
    // compares library sets; list returns every field), so project ALL.
    table.addGlobalSecondaryIndex({
      indexName: GSI2_NAME,
      partitionKey: { name: "researcherAccountId", type: dynamodb.AttributeType.STRING },
      sortKey: { name: "createdAt", type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });

    const gsi1Arn = `${table.tableArn}/index/${GSI1_NAME}`;
    const gsi2Arn = `${table.tableArn}/index/${GSI2_NAME}`;

    // ---------------------------------------------------------------------
    // Lambda functions + tightly-scoped execution roles (Req 16.6, 16.7)
    // ---------------------------------------------------------------------
    // Both functions are Go binaries built to a `bootstrap` executable for the
    // provided.al2023 custom runtime on arm64/Graviton (see
    // scripts/build-lambdas.sh, run via `npm run build:lambdas`). The asset
    // directories under dist/ are produced by that script before synth/deploy.
    const authAssetPath = path.join(__dirname, "..", "dist", "authorization");
    const adminAssetPath = path.join(__dirname, "..", "dist", "admin");

    // Authorization callout role: read-only, and only the GSI1 index it queries
    // (Req 16.7). No table-wide access, no access to GSI2. The callout performs
    // a single dynamodb:Query against GSI1.
    const authRole = new iam.Role(this, "AuthorizationFunctionRole", {
      roleName: `${prefix}-authorization-role`,
      assumedBy: new iam.ServicePrincipal("lambda.amazonaws.com"),
      managedPolicies: [
        iam.ManagedPolicy.fromAwsManagedPolicyName("service-role/AWSLambdaBasicExecutionRole"),
      ],
    });
    authRole.addToPolicy(
      new iam.PolicyStatement({
        sid: "QueryGsi1Only",
        actions: ["dynamodb:Query"],
        resources: [gsi1Arn],
      }),
    );

    const authFn = new lambda.Function(this, "AuthorizationFunction", {
      functionName: `${prefix}-authorization`,
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: "bootstrap",
      code: lambda.Code.fromAsset(authAssetPath),
      role: authRole,
      memorySize: 128,
      timeout: Duration.seconds(10),
      environment: {
        SHARE_RECORDS_TABLE_NAME: table.tableName,
        CACHE_TTL_CEILING: props.cacheTtlCeiling,
        CACHE_DENY_TTL: props.cacheDenyTtl,
      },
    });

    // Admin role: read/write the table, plus Query on GSI2 (the only index it
    // uses — CreateIdempotent and ListByAccount query GSI2; ListAll scans the
    // table; Get/Revoke/Extend Get+Update the table). No access to GSI1, no
    // dynamodb:* wildcard (Req 16.7).
    const adminRole = new iam.Role(this, "AdminFunctionRole", {
      roleName: `${prefix}-admin-role`,
      assumedBy: new iam.ServicePrincipal("lambda.amazonaws.com"),
      managedPolicies: [
        iam.ManagedPolicy.fromAwsManagedPolicyName("service-role/AWSLambdaBasicExecutionRole"),
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

    const adminFn = new lambda.Function(this, "AdminFunction", {
      functionName: `${prefix}-admin`,
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: "bootstrap",
      code: lambda.Code.fromAsset(adminAssetPath),
      role: adminRole,
      memorySize: 128,
      timeout: Duration.seconds(10),
      environment: {
        SHARE_RECORDS_TABLE_NAME: table.tableName,
      },
    });

    // ---------------------------------------------------------------------
    // API Gateway REST API (Req 16.3)
    // ---------------------------------------------------------------------
    // The REST API is retained as the front door for the Authorization_Service
    // Lambdas. Response caching is intentionally NOT enabled anywhere on it:
    // caching of authorization decisions is handled entirely by the
    // standards-governed (RFC 9111) HTTP cache in the htsget-rs authorization
    // callout client, driven by the Cache-Control the Lambda emits.
    const api = new apigw.RestApi(this, "AuthorizationApi", {
      restApiName: `${prefix}-authorization-api`,
      description: "Authorization Service REST API (callout + admin).",
      deployOptions: {
        stageName: props.stageName,
      },
    });

    // Authorization callout: GET /authorize?sourceVpcArn=...  (called by htsget-rs).
    // The forwarded requester identity (the sourceVpcArn query-string parameter)
    // is required, so a response is always specific to one researcher.
    const authorize = api.root.addResource("authorize");
    authorize.addMethod(
      "GET",
      new apigw.LambdaIntegration(authFn, { proxy: true }),
      {
        requestParameters: {
          [`method.request.querystring.${SOURCE_VPC_ARN_PARAM}`]: true,
        },
      },
    );

    // Admin interface (called by the CLI). Paths match the Go admin router
    // (internal/auth/admin.go HandleRequest): the stage strips no path segment
    // beyond the stage name, so the handler sees /admin/shares etc.
    const adminIntegration = new apigw.LambdaIntegration(adminFn, { proxy: true });
    const admin = api.root.addResource("admin");
    const shares = admin.addResource("shares");
    shares.addMethod("POST", adminIntegration); // create
    shares.addMethod("GET", adminIntegration); // list
    shares.addResource("revoke").addMethod("POST", adminIntegration);
    shares.addResource("extend").addMethod("POST", adminIntegration);

    // ---------------------------------------------------------------------
    // Manifest_Bucket + cross-account read policy (Req 16.5, 7.9, 11.5)
    // ---------------------------------------------------------------------
    // A single private bucket in Data Control holding one manifest per
    // researcher under manifests/<account-id>/manifest.json. All public access
    // is blocked. Each onboarded researcher account is granted s3:GetObject on
    // ITS OWN prefix and nothing else. The Lambda/admin path never touches this
    // bucket — only the CLI (via operator credentials) writes it — so no Lambda
    // role is granted any access to it.
    let manifestKey: kms.Key | undefined;
    let bucketEncryption: s3.BucketEncryption;
    if (props.encryption === "kms") {
      manifestKey = new kms.Key(this, "ManifestKey", {
        alias: `alias/${prefix}-manifest`,
        description: "SSE-KMS key for the Data Releaser Manifest_Bucket.",
        enableKeyRotation: true,
        removalPolicy: RemovalPolicy.RETAIN,
      });
      bucketEncryption = s3.BucketEncryption.KMS;
    } else {
      bucketEncryption = s3.BucketEncryption.S3_MANAGED;
    }

    const manifestBucket = new s3.Bucket(this, "ManifestBucket", {
      bucketName: `${prefix}-manifests-${this.account}`,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      publicReadAccess: false,
      enforceSSL: true,
      versioned: true,
      encryption: bucketEncryption,
      encryptionKey: manifestKey,
      removalPolicy: RemovalPolicy.RETAIN,
    });

    // One bucket-policy statement per onboarded researcher account, each scoped
    // to that account's own prefix (Req 7.9 / 16.5). If SSE-KMS is used, the key
    // policy gets a matching per-account kms:Decrypt grant.
    for (const accountId of props.researcherAccountIds) {
      manifestBucket.addToResourcePolicy(
        new iam.PolicyStatement({
          sid: `ResearcherRead${accountId}`,
          effect: iam.Effect.ALLOW,
          principals: [new iam.AccountPrincipal(accountId)],
          actions: ["s3:GetObject"],
          resources: [
            manifestBucket.arnForObjects(`${MANIFEST_PREFIX}/${accountId}/*`),
          ],
        }),
      );

      if (manifestKey) {
        manifestKey.addToResourcePolicy(
          new iam.PolicyStatement({
            sid: `ResearcherDecrypt${accountId}`,
            effect: iam.Effect.ALLOW,
            principals: [new iam.AccountPrincipal(accountId)],
            actions: ["kms:Decrypt"],
            resources: ["*"], // key policy resource is always the key itself
          }),
        );
      }
    }

    // ---------------------------------------------------------------------
    // Optional operator IAM policy (Req 16.9, 11.2, 11.3)
    // ---------------------------------------------------------------------
    // The permissions an operator running the CLI needs (Req 11.2), now
    // including same-account write/delete of the Manifest object. Provisioning
    // this does NOT change Req 11.3: the CLI still creates no IAM at runtime —
    // this is deployment-time infrastructure, attached to operators out of band.
    if (props.provisionOperatorPolicy) {
      const operatorPolicy = new iam.ManagedPolicy(this, "OperatorPolicy", {
        managedPolicyName: `${prefix}-operator`,
        description:
          "Data Releaser operator permissions (Req 11.2). Attach to CLI operators; the CLI creates no IAM itself (Req 11.3).",
        statements: [
          // (a) read-only Athena query of the OrcaVault Mart via Lake Formation.
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
          // (b) read/write Share_Records.
          new iam.PolicyStatement({
            sid: "ShareRecords",
            actions: [
              "dynamodb:GetItem",
              "dynamodb:PutItem",
              "dynamodb:UpdateItem",
              "dynamodb:Query",
              "dynamodb:Scan",
            ],
            resources: [table.tableArn, gsi1Arn, gsi2Arn],
          }),
          // Read the callout API stage, for the `validate` stage check (Req 15.10).
          // Read-only: there is no cache on the stage to invalidate.
          new iam.PolicyStatement({
            sid: "ReadApiStage",
            actions: ["apigateway:GET"],
            resources: [
              `arn:aws:apigateway:${this.region}::/restapis/${api.restApiId}/stages/${props.stageName}`,
            ],
          }),
          // manage VPC Lattice / RAM associations (connectivity preflight).
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
          // (c) same-account write/delete of the Manifest object.
          new iam.PolicyStatement({
            sid: "ManifestWrite",
            actions: ["s3:PutObject", "s3:DeleteObject", "s3:GetObject", "s3:ListBucket"],
            resources: [manifestBucket.bucketArn, manifestBucket.arnForObjects("*")],
          }),
        ],
      });
      if (manifestKey) {
        operatorPolicy.addStatements(
          new iam.PolicyStatement({
            sid: "ManifestKmsEncrypt",
            actions: ["kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey"],
            resources: [manifestKey.keyArn],
          }),
        );
      }

      new CfnOutput(this, "OperatorPolicyArn", {
        value: operatorPolicy.managedPolicyArn,
        description: "ARN of the optional operator IAM policy (Req 11.2).",
      });
    }

    // ---------------------------------------------------------------------
    // Stack outputs (Req 16.4). Named to make the mapping onto the CLI config
    // fields obvious: endpoints.apiGatewayRestApiId, endpoints.apiGatewayStageName,
    // manifest.bucket.
    // ---------------------------------------------------------------------
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
    // The callout URL the CLI/htsget config points at (endpoints.authorization).
    new CfnOutput(this, "AuthorizationCalloutUrl", {
      value: `${api.url}authorize`,
      description: "CLI/htsget config: endpoints.authorization (append ?sourceVpcArn=...)",
    });
  }
}
