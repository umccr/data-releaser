import { CfnOutput, RemovalPolicy, Stack, StackProps } from "aws-cdk-lib";
import * as athena from "aws-cdk-lib/aws-athena";
import * as glue from "aws-cdk-lib/aws-glue";
import * as iam from "aws-cdk-lib/aws-iam";
import * as s3 from "aws-cdk-lib/aws-s3";
import * as s3deploy from "aws-cdk-lib/aws-s3-deployment";
import { Construct } from "constructs";

/**
 * One object in the mock mart.
 */
export interface MockMartObject {
  /**
   * Library ID the CLI is invoked with.
   */
  readonly libraryId: string;
  /**
   * S3 key of the BAM, relative to the data bucket.
   */
  readonly key: string;
}

export interface MockMartStackProps extends StackProps {
  /**
   * Prefix applied to every physical resource name.
   */
  readonly namePrefix: string;

  /**
   * Glue database name.
   */
  readonly databaseName: string;

  /**
   * Athena workgroup name.
   */
  readonly workgroupName: string;

  /**
   * Bucket the advertised objects actually live in.
   */
  readonly dataBucketName: string;

  /**
   * Rows to publish.
   */
  readonly objects: MockMartObject[];

  /**
   * Value of the `cohort_id` column on every row.
   */
  readonly cohortId: string;
}

const TABLE_NAME = "bam";
const TABLE_PREFIX = "bam";
const RESULTS_PREFIX = "athena-results";

/**
 * The mock mart for demos.
 */
export class MockMartStack extends Stack {
  constructor(scope: Construct, id: string, props: MockMartStackProps) {
    super(scope, id, props);

    const prefix = props.namePrefix;
    const bucket = new s3.Bucket(this, "MockMartBucket", {
      bucketName: `${prefix}-mock-mart-${this.account}-${this.region}`,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      encryption: s3.BucketEncryption.S3_MANAGED,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
    });

    const rows = props.objects.map((object, index) =>
      renderRow(object, index, props.dataBucketName, props.cohortId),
    );
    new s3deploy.BucketDeployment(this, "MockMartRows", {
      destinationBucket: bucket,
      destinationKeyPrefix: TABLE_PREFIX,
      sources: [s3deploy.Source.data("rows.json", rows.join("\n") + "\n")],
      prune: true,
    });

    const database = new glue.CfnDatabase(this, "MockMartDatabase", {
      catalogId: this.account,
      databaseInput: {
        name: props.databaseName,
        description: "Mock mart for data releaser demos.",
      },
    });

    const table = new glue.CfnTable(this, "MockMartTable", {
      catalogId: this.account,
      databaseName: props.databaseName,
      tableInput: {
        name: TABLE_NAME,
        description: "Mock mart.bam.",
        tableType: "EXTERNAL_TABLE",
        parameters: {
          classification: "json",
          has_encrypted_data: "false",
        },
        storageDescriptor: {
          location: `s3://${bucket.bucketName}/${TABLE_PREFIX}/`,
          inputFormat: "org.apache.hadoop.mapred.TextInputFormat",
          outputFormat: "org.apache.hadoop.hive.ql.io.HiveIgnoreKeyTextOutputFormat",
          serdeInfo: {
            serializationLibrary: "org.openx.data.jsonserde.JsonSerDe",
            parameters: { "ignore.malformed.json": "true" },
          },
          columns: [
            { name: "portal_run_id", type: "string" },
            { name: "cohort_id", type: "string" },
            { name: "bucket", type: "string" },
            { name: "key", type: "string" },
            { name: "library_id", type: "string" },
            { name: "filename", type: "string" },
            { name: "format", type: "string" },
            { name: "size", type: "bigint" },
            { name: "storage_class", type: "string" },
            { name: "e_tag", type: "string" },
            { name: "last_modified_date", type: "string" },
          ],
        },
      },
    });
    table.addResourceDependency(database);

    const workgroup = new athena.CfnWorkGroup(this, "MockMartWorkgroup", {
      name: props.workgroupName,
      description: "Data Releaser CLI queries against the mock mart.",
      state: "ENABLED",
      recursiveDeleteOption: true,
      workGroupConfiguration: {
        enforceWorkGroupConfiguration: true,
        resultConfiguration: {
          outputLocation: `s3://${bucket.bucketName}/${RESULTS_PREFIX}/`,
          encryptionConfiguration: { encryptionOption: "SSE_S3" },
        },
        publishCloudWatchMetricsEnabled: false,
        engineVersion: { selectedEngineVersion: "AUTO" },
      },
    });

    const policy = new iam.ManagedPolicy(this, "MockMartOperatorPolicy", {
      managedPolicyName: `${prefix}-mock-mart-operator`,
      description: "S3 access the data releaser CLI needs to query the mock mart.",
      statements: [
        new iam.PolicyStatement({
          sid: "MockMartTableRead",
          actions: ["s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation"],
          resources: [bucket.bucketArn, bucket.arnForObjects("*")],
        }),
        new iam.PolicyStatement({
          sid: "MockMartResultsWrite",
          actions: ["s3:PutObject", "s3:AbortMultipartUpload"],
          resources: [bucket.arnForObjects(`${RESULTS_PREFIX}/*`)],
        }),
      ],
    });

    new CfnOutput(this, "MockMartDatabaseName", {
      value: props.databaseName,
      description: "CLI config: warehouse.database",
    });
    new CfnOutput(this, "MockMartWorkgroupName", {
      value: workgroup.name,
      description: "CLI config: warehouse.athenaWorkgroup",
    });
    new CfnOutput(this, "MockMartBucketName", {
      value: bucket.bucketName,
      description: "Bucket holding the mock table data and Athena query results.",
    });
    new CfnOutput(this, "MockMartOperatorPolicyArn", {
      value: policy.managedPolicyArn,
      description: "Attach to CLI operators alongside the main operator policy.",
    });
  }
}

/**
 * Renders a JSON-lines row.
 */
function renderRow(
  object: MockMartObject,
  index: number,
  dataBucketName: string,
  cohortId: string,
): string {
  const key = object.key.replace(/^\/+/, "");
  const filename = key.slice(key.lastIndexOf("/") + 1);
  const dot = filename.lastIndexOf(".");
  const format = dot === -1 ? "" : filename.slice(dot + 1).toLowerCase();

  return JSON.stringify({
    portal_run_id: `20260731${String(index).padStart(8, "0")}`,
    cohort_id: cohortId,
    bucket: dataBucketName,
    key,
    library_id: object.libraryId,
    filename,
    format,
    size: 1339206,
    storage_class: "STANDARD",
    e_tag: String(index).padStart(32, "0"),
    last_modified_date: "2026-07-31 00:00:00",
  });
}
