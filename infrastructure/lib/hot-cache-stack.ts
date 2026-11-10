import * as crypto from "crypto";
import {
  CfnOutput,
  Duration,
  RemovalPolicy,
  Stack,
  StackProps,
  Tags,
  aws_iam as iam,
  aws_kms as kms,
  aws_s3 as s3,
} from "aws-cdk-lib";
import { Construct } from "constructs";

/**
 * Key prefix reserved for the copy orchestration's own artefacts — its start
 * marker and copy summary. The releaser denies the researcher this prefix, so
 * nothing under it may be treated as dataset content.
 */
export const CONTROL_PREFIX = "_control";

/**
 * Shortest lifecycle expiry the stack will accept.
 *
 * The lifecycle rule exists to stop an abandoned copy accruing storage cost
 * indefinitely; it is never how a release stops working. Lifecycle evaluates
 * roughly once a day, rounds up to the following midnight UTC, and deletes
 * objects — so a release re-granted after a lifecycle deletion would need a
 * fresh multi-hour copy. Access expiry lives in the bucket and key policies as
 * a time condition instead, which is why the backstop is required to sit far
 * beyond any plausible release expiry.
 */
const MIN_RETENTION_DAYS = 90;

/**
 * Length in hex characters of the digest embedded in a derived bucket name.
 */
const NAME_DIGEST_CHARS = 16;

/**
 * Longest bucket name the stack derives. S3 allows 63 characters; the slack is
 * reserved for the access-log bucket's `-logs` suffix.
 */
const MAX_DERIVED_NAME = 58;

export interface HotCacheStackProps extends StackProps {
  /**
   * Prefix applied to physical resource names.
   */
  readonly namePrefix: string;

  /**
   * URI identifying the dataset this bucket and key belong to.
   */
  readonly datasetUri: string;

  /**
   * Name of the IAM role the copy orchestration writes through. The key policy
   * names it so the copy can generate data keys for this bucket.
   */
  readonly copyWriterRoleName: string;

  /**
   * Account owning `copyWriterRoleName`. Defaults to this stack's account, for
   * a copy orchestration installed alongside the hot cache.
   *
   * A copy running from another account also needs write access in the bucket
   * policy, which the releaser's policy rendering does not currently produce —
   * the key grant here is necessary but not sufficient for that case.
   */
  readonly copyWriterRoleAccount?: string;

  /**
   * ARN of the principal permitted to disable the key and schedule its
   * deletion — the kill switch for this dataset's cached copy.
   *
   * When omitted the statement is not written, and the kill switch is reachable
   * only through the account's own key administration.
   */
  readonly custodianRoleArn?: string;

  /**
   * Explicit bucket name, bypassing name derivation entirely.
   */
  readonly bucketName?: string;

  /**
   * Secret used to derive the bucket name from the dataset URI with a keyed
   * hash, so the name discloses nothing about the dataset it holds.
   *
   * Without it the name is derived from a legible slug of the dataset URI: an
   * unkeyed digest would be reproducible by anyone who can guess the URI, which
   * is no protection at all, so the choice is between real obscurity and none.
   */
  readonly bucketNameHashKey?: string;

  /**
   * Days after which objects expire, as a cost backstop only. Must be well
   * beyond any release expiry; see {@link MIN_RETENTION_DAYS}.
   *
   * @default 365
   */
  readonly dataRetentionDays?: number;

  /**
   * Days the key remains recoverable after deletion is scheduled. This is the
   * window in which a purge can still be undone.
   *
   * @default 7
   */
  readonly keyPendingWindowDays?: number;

  /**
   * When true, object-level reads are recorded via S3 server access logging so
   * that "what did the researcher actually download" has an answer.
   *
   * Server access logging rather than CloudTrail data events: an account is
   * limited to five trails per region, so a trail per dataset does not scale
   * past five datasets, and a shared trail's event selectors belong to whoever
   * owns that trail rather than to this stack. Access logs are best-effort and
   * delivered with a delay, which is the trade; a shared trail with a prefix
   * selector can be added later without touching this stack.
   *
   * @default true
   */
  readonly logObjectAccess?: boolean;

  /**
   * Existing bucket to receive access logs. When omitted and logging is on, a
   * log bucket is created beside the hot cache bucket.
   *
   * An imported bucket must already permit log delivery from S3; the stack
   * cannot add a policy to a bucket it does not own.
   */
  readonly accessLogsBucketName?: string;
}

/**
 * Derives a bucket name for a dataset.
 *
 * With `hashKey` the name is `<prefix>-dataset-<keyed digest>`, where the digest
 * covers the account, region and dataset URI — so the same dataset in another
 * account or region gets a distinct name, and the name itself reveals nothing.
 *
 * Without `hashKey` the name is a legible slug of the dataset URI with a short
 * digest appended to keep truncated slugs distinct.
 */
export function hotCacheBucketName(opts: {
  namePrefix: string;
  datasetUri: string;
  account: string;
  region: string;
  hashKey?: string;
}): string {
  const scoped = `${opts.account}/${opts.region}/${opts.datasetUri}`;

  if (opts.hashKey) {
    const digest = crypto
      .createHmac("sha256", opts.hashKey)
      .update(scoped)
      .digest("hex")
      .slice(0, NAME_DIGEST_CHARS);
    return `${opts.namePrefix}-dataset-${digest}`;
  }

  const digest = crypto
    .createHash("sha256")
    .update(scoped)
    .digest("hex")
    .slice(0, 8);
  const head = `${opts.namePrefix}-dataset-`;
  const tail = `-${digest}`;
  const slug = slugify(opts.datasetUri);
  const room = MAX_DERIVED_NAME - head.length - tail.length;

  return `${head}${slug.slice(0, room) || "unnamed"}${tail}`;
}

/**
 * Reduces a dataset URI to the character set S3 bucket names allow.
 */
function slugify(datasetUri: string): string {
  return datasetUri
    .replace(/^[a-zA-Z0-9+.-]+:\/\//, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

/**
 * One dataset's hot cache: a single bucket holding the copy released to
 * researchers, encrypted with a customer-managed key used by nothing else.
 *
 * The key is the unit of destruction. Scheduling its deletion renders this
 * dataset's cached copy permanently unreadable and touches no other dataset,
 * which is why there is one key — and one stack — per dataset rather than a
 * shared key across many.
 *
 * Grants to researchers are not deployed here. The releaser writes them into
 * the bucket and key policies when a release is made, and rewrites both
 * documents whole on every change, so this stack establishes only the baseline
 * those documents are built on.
 */
export class HotCacheStack extends Stack {
  /**
   * Name of the bucket holding this dataset's cached copy.
   */
  readonly bucketName: string;

  /**
   * ARN of the key encrypting that bucket and nothing else.
   */
  readonly keyArn: string;

  constructor(scope: Construct, id: string, props: HotCacheStackProps) {
    super(scope, id, props);

    if (!props.datasetUri.trim()) {
      throw new Error("datasetUri is required");
    }

    const retentionDays = props.dataRetentionDays ?? 365;
    if (retentionDays < MIN_RETENTION_DAYS) {
      throw new Error(
        `dataRetentionDays must be at least ${MIN_RETENTION_DAYS}: the lifecycle rule is a ` +
          "storage-cost backstop, and access expiry is enforced by a time condition in the " +
          "bucket and key policies rather than by deleting objects",
      );
    }

    const bucketName =
      props.bucketName ??
      hotCacheBucketName({
        namePrefix: props.namePrefix,
        datasetUri: props.datasetUri,
        account: this.account,
        region: this.region,
        hashKey: props.bucketNameHashKey,
      });

    const key = new kms.Key(this, "DatasetKey", {
      alias: `alias/${bucketName}`,
      description: `Encrypts the hot cache copy of one dataset in ${bucketName}. Scheduling deletion of this key destroys that copy.`,
      enableKeyRotation: true,
      pendingWindow: Duration.days(props.keyPendingWindowDays ?? 7),
      removalPolicy: RemovalPolicy.RETAIN,
    });

    // The copy orchestration writes objects through its own role, so S3 asks KMS
    // for a data key on its behalf. The encryption context condition ties that
    // permission to this bucket's objects, so the role cannot use the key to
    // write anywhere else.
    const copyWriterAccount = props.copyWriterRoleAccount ?? this.account;
    key.addToResourcePolicy(
      new iam.PolicyStatement({
        sid: "AllowCopyWriterToEncrypt",
        effect: iam.Effect.ALLOW,
        principals: [
          new iam.ArnPrincipal(
            `arn:aws:iam::${copyWriterAccount}:role/${props.copyWriterRoleName}`,
          ),
        ],
        actions: [
          "kms:GenerateDataKey",
          "kms:GenerateDataKeyWithoutPlaintext",
          "kms:Decrypt",
          "kms:DescribeKey",
        ],
        resources: ["*"],
        conditions: {
          StringLike: {
            "kms:EncryptionContext:aws:s3:arn": `arn:aws:s3:::${bucketName}/*`,
          },
        },
      }),
    );

    // The kill switch. Cancelling a scheduled deletion is deliberately absent:
    // the custodian can stop access and destroy the copy, but undoing a purge
    // needs the account's key administrators, not the custodian alone.
    if (props.custodianRoleArn) {
      key.addToResourcePolicy(
        new iam.PolicyStatement({
          sid: "AllowCustodianToWithdraw",
          effect: iam.Effect.ALLOW,
          principals: [new iam.ArnPrincipal(props.custodianRoleArn)],
          actions: ["kms:DisableKey", "kms:ScheduleKeyDeletion", "kms:DescribeKey"],
          resources: ["*"],
        }),
      );
    }

    const accessLogs = this.accessLogsBucket(props, bucketName);

    const bucket = new s3.Bucket(this, "HotCacheBucket", {
      bucketName,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      objectOwnership: s3.ObjectOwnership.BUCKET_OWNER_ENFORCED,
      // enforceSSL contributes the insecure-transport deny. The releaser
      // reproduces that statement whenever it rewrites this policy, so it is
      // the only baseline statement that survives a release.
      enforceSSL: true,
      encryption: s3.BucketEncryption.KMS,
      encryptionKey: key,
      // S3 Bucket Keys must stay off. With them on, S3 caches a bucket-level
      // key and subsequent reads neither call KMS nor re-check the key policy,
      // which would leave a window where a disabled or pending-deletion key
      // still yields readable objects and would make the expiry condition on
      // the key policy unreliable. The cost is one decrypt call per object
      // read, which is negligible for whole-object reads.
      bucketKeyEnabled: false,
      versioned: false,
      // A non-empty bucket cannot be deleted, and the objects here are a copy a
      // researcher may still be reading. Removal is deliberate and manual.
      removalPolicy: RemovalPolicy.RETAIN,
      serverAccessLogsBucket: accessLogs,
      serverAccessLogsPrefix: accessLogs ? `${bucketName}/` : undefined,
      lifecycleRules: [
        {
          id: "ExpireCachedData",
          enabled: true,
          expiration: Duration.days(retentionDays),
        },
        {
          id: "CleanupIncompleteMultipartUploads",
          enabled: true,
          abortIncompleteMultipartUploadAfter: Duration.days(7),
        },
      ],
    });

    this.bucketName = bucket.bucketName;
    this.keyArn = key.keyArn;

    // A keyed bucket name is pointless if a tag spells the dataset out beside
    // it, so the URI is tagged only when no obscurity was asked for. The ledger
    // holds the mapping from dataset URI to bucket either way.
    if (!props.bucketNameHashKey) {
      Tags.of(bucket).add("DatasetUri", props.datasetUri);
      Tags.of(key).add("DatasetUri", props.datasetUri);
    }

    new CfnOutput(this, "HotCacheBucketName", {
      value: bucket.bucketName,
      description: "Bucket holding this dataset's cached copy.",
    });
    new CfnOutput(this, "HotCacheBucketArn", {
      value: bucket.bucketArn,
      description: "ARN of the hot cache bucket.",
    });
    new CfnOutput(this, "DatasetKeyArn", {
      value: key.keyArn,
      description: "ARN of the key encrypting this dataset's cached copy.",
    });
    new CfnOutput(this, "DatasetKeyId", {
      value: key.keyId,
      description:
        "Key id for withdrawal: disabling or scheduling deletion of this key destroys the cached copy.",
    });
    new CfnOutput(this, "ControlPrefix", {
      value: `${CONTROL_PREFIX}/`,
      description:
        "Prefix reserved for copy artefacts, denied to researchers and never dataset content.",
    });
    if (accessLogs) {
      new CfnOutput(this, "AccessLogsBucketName", {
        value: accessLogs.bucketName,
        description: "Bucket receiving object access logs for the hot cache bucket.",
      });
    }
  }

  /**
   * Resolves where object access logs go, if anywhere.
   *
   * Logs must not land in the hot cache bucket itself: a released researcher can
   * read that bucket, and access logs would then be readable by the party they
   * describe.
   */
  private accessLogsBucket(
    props: HotCacheStackProps,
    bucketName: string,
  ): s3.IBucket | undefined {
    if (!(props.logObjectAccess ?? true)) {
      return undefined;
    }

    if (props.accessLogsBucketName) {
      return s3.Bucket.fromBucketName(this, "AccessLogsBucket", props.accessLogsBucketName);
    }

    if (bucketName.length > MAX_DERIVED_NAME) {
      throw new Error(
        `bucketName is too long to derive a log bucket name from (max ${MAX_DERIVED_NAME} ` +
          "characters); pass accessLogsBucketName or turn logObjectAccess off",
      );
    }

    // S3-managed encryption, because log delivery cannot use the dataset key —
    // and must not, since the key is destroyed on purge while the record of who
    // read what should outlive the data.
    // Object ownership is left to the S3 construct: how log delivery is granted
    // — an ACL grant or a bucket policy — depends on the app's feature flags,
    // and only one of those two settings is compatible with each.
    return new s3.Bucket(this, "AccessLogsBucket", {
      bucketName: `${bucketName}-logs`,
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      encryption: s3.BucketEncryption.S3_MANAGED,
      removalPolicy: RemovalPolicy.RETAIN,
      lifecycleRules: [{ id: "ExpireAccessLogs", enabled: true, expiration: Duration.days(400) }],
    });
  }
}
