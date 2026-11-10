import { App, Environment } from "aws-cdk-lib";
import { HotCacheStack } from "./hot-cache-stack";

/**
 * Stack names accept letters, digits and hyphens and must begin with a letter.
 * A dataset's configured name becomes its stack name, so it is held to the same
 * rule at synth time rather than failing at deploy.
 */
const STACK_NAME = /^[A-Za-z][A-Za-z0-9-]*$/;

/**
 * Environment variable carrying the secret that keys bucket-name derivation.
 *
 * Not a context value: the context file is checked in, and a checked-in key
 * obscures nothing.
 */
export const BUCKET_NAME_HASH_KEY_VAR = "HOT_CACHE_BUCKET_NAME_HASH_KEY";

/**
 * Resolves the bucket-name hash key, refusing to continue without one when
 * obscured names were asked for.
 *
 * Refusing is the point of the flag. Were the key merely absent, names would
 * fall back to their legible form — a different name, so a different bucket. The
 * deployed bucket would be left behind, since its removal policy retains it, and
 * an empty replacement would take its place while the released copy sat in the
 * bucket nothing now refers to.
 */
export function resolveBucketNameHashKey(
  obscureBucketNames: boolean,
  env: Record<string, string | undefined>,
): string | undefined {
  if (!obscureBucketNames) {
    return undefined;
  }

  const hashKey = env[BUCKET_NAME_HASH_KEY_VAR];
  if (!hashKey) {
    throw new Error(
      `obscureBucketNames is set, so ${BUCKET_NAME_HASH_KEY_VAR} must carry the key that derives ` +
        "bucket names. It has to be the same value on every synth, or a deployment renames the " +
        "bucket and abandons the data in the one it replaces",
    );
  }
  return hashKey;
}

/**
 * One dataset that gets a bucket and key of its own.
 */
export interface DatasetConfig {
  /**
   * Short operator-chosen handle for this dataset, unique within the account and
   * stable for the life of the deployment. It names the stack, so it is what
   * `cdk deploy` and `cdk destroy` are given.
   *
   * Legible on purpose, even when bucket names are obscured: stack names are
   * visible only inside the account that owns the data, never to the party the
   * data is released to.
   */
  readonly name: string;

  /**
   * URI identifying the dataset held in this bucket.
   */
  readonly datasetUri: string;

  /**
   * Explicit bucket name, bypassing name derivation.
   *
   * Worth setting once a bucket holds a copy someone is reading: a derived name
   * depends on inputs that may change, and a changed name is a new bucket.
   */
  readonly bucketName?: string;

  /**
   * Overrides the account-wide storage-cost backstop for this dataset.
   */
  readonly dataRetentionDays?: number;
}

/**
 * Settings shared by every dataset in one account and region.
 */
export interface HotCacheStacksOptions {
  /**
   * Account and region every dataset stack is deployed to.
   */
  readonly env: Environment;

  /**
   * Prefix applied to physical resource names.
   */
  readonly namePrefix: string;

  /**
   * Name of the role the copy orchestration writes through. Required as soon as
   * one dataset is configured, because each dataset's key policy has to name it.
   */
  readonly copyWriterRoleName?: string;

  /**
   * Account owning that role, when the copy orchestration runs elsewhere.
   */
  readonly copyWriterRoleAccount?: string;

  /**
   * ARN of the principal allowed to disable a dataset key and schedule its
   * deletion. Omitted, that power rests with the account's key administrators.
   */
  readonly custodianRoleArn?: string;

  /**
   * Secret keying the derivation of bucket names from dataset URIs.
   */
  readonly bucketNameHashKey?: string;

  /**
   * Days after which objects expire, as a storage-cost backstop only.
   */
  readonly dataRetentionDays?: number;

  /**
   * Days a dataset key stays recoverable after deletion is scheduled.
   */
  readonly keyPendingWindowDays?: number;

  /**
   * Whether object reads are recorded.
   */
  readonly logObjectAccess?: boolean;

  /**
   * Existing bucket to receive access logs for every dataset. Omitted, each
   * dataset stack creates its own log bucket.
   */
  readonly accessLogsBucketName?: string;

  /**
   * The datasets to deploy. An empty list deploys none.
   */
  readonly datasets: readonly DatasetConfig[];
}

/**
 * Stack name for a dataset, derived from its configured handle.
 */
export function hotCacheStackName(datasetName: string): string {
  return `HotCache-${datasetName}`;
}

/**
 * Adds one stack per configured dataset.
 *
 * Deployment is deliberately out of band: an operator deploys a dataset's stack
 * by name before staging it, and the CLI only checks that the bucket and key
 * exist, reporting the deployment that owns them when they do not. The CLI would
 * otherwise need a Node and CDK toolchain plus deployment-grade credentials on
 * the path that grants access, and creating a bucket and key is not something a
 * release command should be able to do as a side effect.
 */
export function hotCacheStacks(app: App, opts: HotCacheStacksOptions): HotCacheStack[] {
  if (opts.datasets.length === 0) {
    return [];
  }

  const copyWriterRoleName = opts.copyWriterRoleName;
  if (!copyWriterRoleName) {
    throw new Error(
      "copyWriterRoleName is required once a dataset is configured: it names the role the copy " +
        "orchestration writes through, which each dataset's key policy has to grant. The copy " +
        "orchestration is deployed separately and fixes that role's name",
    );
  }

  const names = new Set<string>();
  const uris = new Set<string>();

  return opts.datasets.map((dataset) => {
    if (typeof dataset.name !== "string" || !STACK_NAME.test(dataset.name)) {
      throw new Error(
        `dataset name ${JSON.stringify(dataset.name)} must start with a letter and hold only ` +
          "letters, digits and hyphens, because it becomes the stack name",
      );
    }
    if (typeof dataset.datasetUri !== "string" || !dataset.datasetUri.trim()) {
      throw new Error(`dataset ${dataset.name} is missing datasetUri`);
    }
    if (names.has(dataset.name)) {
      throw new Error(`dataset name ${dataset.name} is configured twice; each names one stack`);
    }
    // Two entries sharing a URI would derive one bucket name and contend for it,
    // with whichever stack deployed second failing on a name already taken.
    if (uris.has(dataset.datasetUri)) {
      throw new Error(
        `dataset URI ${dataset.datasetUri} is configured twice, under ${dataset.name} and ` +
          "another name; one dataset gets one bucket",
      );
    }
    names.add(dataset.name);
    uris.add(dataset.datasetUri);

    return new HotCacheStack(app, hotCacheStackName(dataset.name), {
      env: opts.env,
      namePrefix: opts.namePrefix,
      datasetUri: dataset.datasetUri,
      copyWriterRoleName,
      copyWriterRoleAccount: opts.copyWriterRoleAccount,
      custodianRoleArn: opts.custodianRoleArn,
      bucketName: dataset.bucketName,
      bucketNameHashKey: opts.bucketNameHashKey,
      dataRetentionDays: dataset.dataRetentionDays ?? opts.dataRetentionDays,
      keyPendingWindowDays: opts.keyPendingWindowDays,
      logObjectAccess: opts.logObjectAccess,
      accessLogsBucketName: opts.accessLogsBucketName,
      // The dataset URI is left out: a stack description is one more place it
      // would appear beside a name meant to obscure it.
      description: `Hot cache bucket and key holding one dataset's released copy (${dataset.name}).`,
    });
  });
}
