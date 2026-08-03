import { CfnOutput, RemovalPolicy, Stack, StackProps } from "aws-cdk-lib";
import * as athena from "aws-cdk-lib/aws-athena";
import * as glue from "aws-cdk-lib/aws-glue";
import * as iam from "aws-cdk-lib/aws-iam";
import * as s3 from "aws-cdk-lib/aws-s3";
import * as s3deploy from "aws-cdk-lib/aws-s3-deployment";
import { Construct } from "constructs";

/** One object the mock Mart advertises as a shareable read. */
export interface MockMartObject {
  /** Library ID the CLI is invoked with (`--libraries`). */
  readonly libraryId: string;
  /** S3 key of the BAM, relative to the data bucket (no leading slash). */
  readonly key: string;
}

export interface MockMartStackProps extends StackProps {
  /** Prefix applied to every physical resource name. */
  readonly namePrefix: string;

  /**
   * Glue database name. Must match `warehouse.database` in the CLI config —
   * the resolution query is literally `... FROM <database>.bam AS b ...`.
   */
  readonly databaseName: string;

  /** Athena workgroup name. Must match `warehouse.athenaWorkgroup` in the CLI config. */
  readonly workgroupName: string;

  /**
   * Bucket the advertised objects actually live in. This stack does NOT create
   * or write to it: htsget-rs serves the reads from there, and only the row's
   * `bucket` column names it.
   */
  readonly dataBucketName: string;

  /** Rows to publish. */
  readonly objects: MockMartObject[];

  /** Value of the `cohort_id` column on every row. */
  readonly cohortId: string;
}

/** Glue/Athena require lowercase identifiers for databases and tables. */
const TABLE_NAME = "bam";

/** Prefix under the mock bucket holding the table's data files. */
const TABLE_PREFIX = "bam";

/** Prefix under the mock bucket Athena writes query results to. */
const RESULTS_PREFIX = "athena-results";

/**
 * A stand-in for the OrcaVault Mart, for demos and for exercising the CLI
 * end-to-end without access to the real warehouse.
 *
 * It provisions exactly what `internal/warehouse` needs to resolve libraries: a
 * Glue database with a `bam` table whose columns are the ones the resolution
 * query selects, backed by JSON-lines rows this stack writes, plus an Athena
 * workgroup to run it in.
 *
 * The workgroup carries a `resultConfiguration.outputLocation` and enforces it.
 * That is not cosmetic: the CLI submits `StartQueryExecution` with no
 * `ResultConfiguration` of its own (internal/warehouse/executor.go), so a
 * workgroup without an output location fails every query with
 * "No output location provided".
 *
 * This stack is optional and off by default — it is deployed only when the
 * `mockMart` context flag is set. It is entirely disposable: both the bucket
 * and its contents are destroyed with the stack.
 */
export class MockMartStack extends Stack {
  constructor(scope: Construct, id: string, props: MockMartStackProps) {
    super(scope, id, props);

    const prefix = props.namePrefix;

    // ---------------------------------------------------------------------
    // Bucket holding the table data and the Athena query results.
    // ---------------------------------------------------------------------
    // Disposable by design: this holds mock metadata and query scratch output,
    // never anything worth retaining. The two prefixes are kept in one bucket
    // so tearing the mock down is a single delete.
    const bucket = new s3.Bucket(this, "MockMartBucket", {
      bucketName: `${prefix}-mock-mart-${this.account}-${this.region}`,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      encryption: s3.BucketEncryption.S3_MANAGED,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
    });

    // ---------------------------------------------------------------------
    // Table data: one JSON object per line (org.openx.data.jsonserde.JsonSerDe).
    // ---------------------------------------------------------------------
    const rows = props.objects.map((object, index) =>
      renderRow(object, index, props.dataBucketName, props.cohortId),
    );

    // Pruning is scoped to destinationKeyPrefix, so this never touches the
    // Athena results written under RESULTS_PREFIX in the same bucket.
    new s3deploy.BucketDeployment(this, "MockMartRows", {
      destinationBucket: bucket,
      destinationKeyPrefix: TABLE_PREFIX,
      sources: [s3deploy.Source.data("rows.json", rows.join("\n") + "\n")],
      prune: true,
    });

    // ---------------------------------------------------------------------
    // Glue database + external table.
    // ---------------------------------------------------------------------
    const database = new glue.CfnDatabase(this, "MockMartDatabase", {
      catalogId: this.account,
      databaseInput: {
        name: props.databaseName,
        description: "Mock OrcaVault Mart for Data Releaser demos (not the real warehouse).",
      },
    });

    // The column set is the contract with internal/warehouse: the resolution
    // query selects library_id, portal_run_id, bucket, key, format and
    // storage_class, and filters on lower(format) = 'bam'. The remaining
    // columns mirror the real Mart so the mock is a drop-in for it.
    const table = new glue.CfnTable(this, "MockMartTable", {
      catalogId: this.account,
      databaseName: props.databaseName,
      tableInput: {
        name: TABLE_NAME,
        description: "Mock mart.bam — one row per shareable read.",
        tableType: "EXTERNAL_TABLE",
        parameters: {
          classification: "json",
          // Rows are one-per-line JSON; there is no header to skip.
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
            // Kept as a string rather than a timestamp: JsonSerDe only parses
            // Hive's own timestamp literal, and nothing in the CLI reads it.
            { name: "last_modified_date", type: "string" },
          ],
        },
      },
    });
    table.addDependency(database);

    // ---------------------------------------------------------------------
    // Athena workgroup.
    // ---------------------------------------------------------------------
    const workgroup = new athena.CfnWorkGroup(this, "MockMartWorkgroup", {
      name: props.workgroupName,
      description: "Data Releaser CLI queries against the mock Mart.",
      state: "ENABLED",
      // Lets `cdk destroy` remove the workgroup even after queries have run.
      recursiveDeleteOption: true,
      workGroupConfiguration: {
        // enforceWorkGroupConfiguration makes this the output location for every
        // query in the workgroup, including ones (like the CLI's) that supply none.
        enforceWorkGroupConfiguration: true,
        resultConfiguration: {
          outputLocation: `s3://${bucket.bucketName}/${RESULTS_PREFIX}/`,
          encryptionConfiguration: { encryptionOption: "SSE_S3" },
        },
        publishCloudWatchMetricsEnabled: false,
        engineVersion: { selectedEngineVersion: "AUTO" },
      },
    });

    // ---------------------------------------------------------------------
    // Operator permissions for the mock.
    // ---------------------------------------------------------------------
    // The main stack's operator policy covers athena:* and glue:* actions, but
    // not S3 on this bucket — and Athena reads the table data and writes results
    // with the *caller's* credentials, so without these the query fails on
    // access denied rather than on anything to do with the query itself.
    const policy = new iam.ManagedPolicy(this, "MockMartOperatorPolicy", {
      managedPolicyName: `${prefix}-mock-mart-operator`,
      description: "S3 access the Data Releaser CLI needs to query the mock Mart.",
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

    // ---------------------------------------------------------------------
    // Outputs, named after the CLI config fields they populate.
    // ---------------------------------------------------------------------
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
 * Renders one JSON-lines row.
 *
 * Every derived value is a pure function of the object and its index. Nothing
 * here may depend on wall-clock time or randomness: two synths of the same input
 * must produce byte-identical rows, or every deploy would churn the asset hash.
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
    // Must not be an archived class: the CLI refuses to share those (Req 1.7).
    storage_class: "STANDARD",
    e_tag: String(index).padStart(32, "0"),
    last_modified_date: "2026-07-31 00:00:00",
  });
}
