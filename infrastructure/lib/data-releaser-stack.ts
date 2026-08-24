import * as path from "path";
import {
  Aspects,
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
  aws_ec2 as ec2,
} from "aws-cdk-lib";
import { Construct } from "constructs";
import { VpcOnlyAspect } from "./vpc-only-aspect";

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

  /**
   * VPC id hosting the `execute-api` interface endpoint through which the
   * authorization callout reaches this API, and into which both Lambda
   * functions place their ENIs. Always required: there is no public variant of
   * this stack.
   *
   * `GET /authorize` cannot be authenticated — htsget-rs issues the callout as
   * an unsigned GET — so network reachability is the only control available for
   * it. The REST API is therefore ALWAYS a PRIVATE endpoint; the former
   * `privateApi=false` sandbox escape hatch has been removed deliberately, and
   * the VpcOnlyAspect fails synth on any non-private API or non-VPC function.
   *
   * This must be the SAME VPC the htsget-rs Lambda is attached to
   * (`vpcOrName` on the htsget-pipeline lattice producer) — a private API is
   * reachable only through an interface endpoint in a VPC the caller has ENIs
   * in. It is looked up rather than created: it is a pre-existing landing-zone
   * VPC that this stack must not own or modify.
   */
  readonly apiVpcId: string;
}

/** DynamoDB GSI names — must match the constants the Go handlers query against. */
const GSI1_NAME = "GSI1"; // targetVpcArn — authorization callout lookup (Req 4.2)
const GSI2_NAME = "GSI2"; // researcherAccountId + createdAt — list / idempotency

/**
 * Header carrying the requester identity (source VPC ARN).
 *
 * htsget-rs issues the authorization callout as a bare GET and passes every
 * forwarded context value as a header prefixed with `Htsget-Context-`, so the
 * identity never appears in the query string. The name after the prefix is the
 * `name` of the `auth.authorization.forward.context.extensions` entry that
 * extracts `$.requestContext.identity.sourceVpcArn`.
 */
const SOURCE_VPC_ARN_HEADER = "Htsget-Context-SourceVpcArn";

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

    // Synth-time invariant, not convention: any Lambda without a VpcConfig, any
    // non-PRIVATE API Gateway, any HTTP API, any function URL, any public
    // Lambda permission and any bucket without a full public-access block fails
    // the build. The stack enforces this on itself so that every consumer —
    // the CDK app, the assertion tests, anything else that instantiates it —
    // gets the guarantee, not just deploys that go through bin/.
    Aspects.of(this).add(new VpcOnlyAspect());

    // ---------------------------------------------------------------------
    // VPC (pre-existing landing-zone VPC, looked up — never created/modified
    // beyond adding ENIs, security groups and the interface endpoint).
    // ---------------------------------------------------------------------
    // Everything with a network interface in this stack lives here: the
    // `execute-api` interface endpoint the callout arrives through, and the
    // ENIs of both Lambda functions.
    const vpc = ec2.Vpc.fromLookup(this, "ApiVpc", { vpcId: props.apiVpcId });

    // Egress-only security group for the two Lambda functions. They accept no
    // inbound connections (Lambda invocation is an AWS control-plane operation,
    // not a network connection to the ENI) and only ever open HTTPS outwards —
    // to DynamoDB via the VPC's private egress path.
    const lambdaSecurityGroup = new ec2.SecurityGroup(this, "LambdaSg", {
      vpc,
      description: `${prefix} Lambda functions: HTTPS egress only.`,
      allowAllOutbound: false,
    });
    lambdaSecurityGroup.addEgressRule(
      ec2.Peer.anyIpv4(),
      ec2.Port.tcp(443),
      "HTTPS to AWS service endpoints (DynamoDB)",
    );

    // Both functions sit in the private-with-egress subnets: they need a route
    // to the regional DynamoDB endpoint (NAT, or a DynamoDB gateway endpoint if
    // the landing-zone VPC provides one). CloudWatch logging is unaffected by
    // VPC attachment — log delivery does not traverse the function's ENIs.
    const lambdaVpcConfig = {
      vpc,
      vpcSubnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
      securityGroups: [lambdaSecurityGroup],
    };

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
    // scripts/build-lambdas.sh, run via `pnpm run build:lambdas`). The asset
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
        // Superset of AWSLambdaBasicExecutionRole: the same CloudWatch Logs
        // permissions plus the ENI create/describe/delete that a VPC-attached
        // function needs.
        iam.ManagedPolicy.fromAwsManagedPolicyName(
          "service-role/AWSLambdaVPCAccessExecutionRole",
        ),
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
      ...lambdaVpcConfig,
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
        // See the authorization role: VPC variant of the basic execution role.
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

    const adminFn = new lambda.Function(this, "AdminFunction", {
      functionName: `${prefix}-admin`,
      runtime: lambda.Runtime.PROVIDED_AL2023,
      architecture: lambda.Architecture.ARM_64,
      handler: "bootstrap",
      code: lambda.Code.fromAsset(adminAssetPath),
      role: adminRole,
      memorySize: 128,
      timeout: Duration.seconds(10),
      ...lambdaVpcConfig,
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
    // The callout itself cannot be authenticated: htsget-rs sends an unsigned
    // GET and its config has no way to attach a credential (the callout's
    // `forward.headers` is an allow/deny filter over the *client's* headers, not
    // a way to inject one). Reachability is therefore the control: the API is a
    // PRIVATE endpoint, so the only way in is an `execute-api` interface
    // endpoint in the VPC the htsget-rs Lambda has its ENIs in. It is not
    // resolvable, let alone callable, from the internet.
    //
    // Private DNS on the endpoint keeps the hostname unchanged
    // (`<api-id>.execute-api.<region>.amazonaws.com`), so the callout URL and
    // the whole HTSGET_AUTH_AUTHORIZATION string are identical to the public
    // form — nothing in htsget-rs's config changes.
    // Only the VPC's own address space may open the endpoint, and only on 443.
    // The endpoint is in a shared landing-zone VPC, so this security group is
    // one of the few pieces of it this stack owns.
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
      // Resolves the standard execute-api hostname to the endpoint inside the
      // VPC, which is what lets the callout URL stay unchanged.
      privateDnsEnabled: true,
      securityGroups: [endpointSecurityGroup],
      subnets: { subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS },
    });

    // A private API is deny-by-default: without a resource policy nothing can
    // call it at all. The allow is conditioned on the endpoint the request came
    // through, and the explicit deny makes that a hard boundary rather than
    // something a future identity-policy grant could widen.
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

    // Authorization callout: GET /authorize (called by htsget-rs).
    //
    // The requester identity arrives in the ${SOURCE_VPC_ARN_HEADER} header, not
    // the query string. It is deliberately NOT declared as a required request
    // parameter: API Gateway rejects a request missing a required parameter with a
    // 400 before the Lambda runs, so requiring anything htsget-rs does not send
    // makes every callout fail. The handler enforces the identity instead —
    // a request without it matches no share and is denied.
    const authorize = api.root.addResource("authorize");
    authorize.addMethod("GET", new apigw.LambdaIntegration(authFn, { proxy: true }));

    // Admin interface (called by the CLI). Paths match the Go admin router
    // (internal/auth/admin.go HandleRequest): the stage strips no path segment
    // beyond the stage name, so the handler sees /admin/shares etc.
    //
    // Every admin method requires SigV4 (AuthorizationType.IAM). These routes
    // create, revoke and extend shares, so an unauthenticated one would let
    // anyone who learns the URL grant themselves access to the data bucket.
    // Callers need execute-api:Invoke on the method ARN — the operator managed
    // policy below grants exactly that when `provisionOperatorPolicy` is set.
    //
    // GET /authorize above cannot be given the same treatment — htsget-rs issues
    // the callout as a plain unsigned GET, so IAM auth there would reject every
    // request. Its control is the private endpoint above: unauthenticated, but
    // only reachable from inside the VPC. The two layers compose, so an admin
    // call must both arrive through the interface endpoint and be signed by a
    // principal holding execute-api:Invoke.
    const adminIntegration = new apigw.LambdaIntegration(adminFn, { proxy: true });
    const adminAuth: apigw.MethodOptions = {
      authorizationType: apigw.AuthorizationType.IAM,
    };
    const admin = api.root.addResource("admin");
    const shares = admin.addResource("shares");
    shares.addMethod("POST", adminIntegration, adminAuth); // create
    shares.addMethod("GET", adminIntegration, adminAuth); // list
    shares.addResource("revoke").addMethod("POST", adminIntegration, adminAuth);
    shares.addResource("extend").addMethod("POST", adminIntegration, adminAuth);

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
          // Call the admin API. Those methods are AuthorizationType.IAM, so a
          // caller without this statement gets 403 Missing Authentication Token
          // no matter how well it signs the request. Scoped to /admin/* on this
          // stage only: it deliberately does not cover GET /authorize, which is
          // open and needs no permission.
          new iam.PolicyStatement({
            sid: "InvokeAdminApi",
            actions: ["execute-api:Invoke"],
            resources: [api.arnForExecuteApi("*", "/admin/*", props.stageName)],
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
    // Unchanged by the private endpoint: private DNS resolves this same hostname
    // to the interface endpoint inside the VPC. It only resolves there, though —
    // it is not callable from an operator workstation.
    new CfnOutput(this, "AuthorizationCalloutUrl", {
      value: `${api.url}authorize`,
      description:
        "CLI/htsget config: endpoints.authorization (identity travels in the " +
        `${SOURCE_VPC_ARN_HEADER} header)`,
    });
    new CfnOutput(this, "AuthorizationApiVpcEndpointId", {
      value: vpcEndpoint.vpcEndpointId,
      description:
        "The only route to the authorization API. Callers must have ENIs in " +
        "this VPC; everything else is denied by the API resource policy.",
    });
  }
}
