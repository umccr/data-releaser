import * as cdk from "aws-cdk-lib";
import { Template } from "aws-cdk-lib/assertions";
import {
  CONTROL_PREFIX,
  HotCacheStack,
  HotCacheStackProps,
  hotCacheBucketName,
} from "../src/hot-cache-stack";
import {
  BUCKET_NAME_HASH_KEY_VAR,
  DatasetConfig,
  HotCacheStacksOptions,
  hotCacheStackName,
  hotCacheStacks,
  resolveBucketNameHashKey,
} from "../src/hot-cache-stacks";

const ACCOUNT = "000000000000";
const REGION = "ap-southeast-2";
const NAME_PREFIX = "data-releaser";
const DATASET_URI = "s3://ceph-source/datasets/demo";
const COPY_WRITER_ROLE = "steps-s3-copy-writer";
const CUSTODIAN_ARN = `arn:aws:iam::${ACCOUNT}:role/data-releaser-custodian`;

/**
 * Longest name the stack derives, and the ceiling above which it can no longer
 * append `-logs` to reach a log bucket name.
 */
const MAX_DERIVED_NAME = 58;

function buildStack(overrides: Partial<HotCacheStackProps> = {}): HotCacheStack {
  const app = new cdk.App();
  return new HotCacheStack(app, "HotCache-Demo", {
    env: { account: ACCOUNT, region: REGION },
    namePrefix: NAME_PREFIX,
    datasetUri: DATASET_URI,
    copyWriterRoleName: COPY_WRITER_ROLE,
    custodianRoleArn: CUSTODIAN_ARN,
    ...overrides,
  });
}

function synth(overrides: Partial<HotCacheStackProps> = {}): Template {
  return Template.fromStack(buildStack(overrides));
}

/**
 * The bucket holding the dataset copy, as distinct from the log bucket beside
 * it. Returned with its logical id so references to it can be recognised.
 */
function cacheBucket(t: Template): [string, any] {
  const entries = Object.entries(t.findResources("AWS::S3::Bucket")).filter(
    ([, b]) => !String((b as any).Properties.BucketName ?? "").endsWith("-logs"),
  );
  expect(entries).toHaveLength(1);
  return entries[0] as [string, any];
}

function logBucket(t: Template): any {
  const found = Object.values(t.findResources("AWS::S3::Bucket")).filter((b) =>
    String((b as any).Properties.BucketName ?? "").endsWith("-logs"),
  );
  expect(found).toHaveLength(1);
  return found[0];
}

function datasetKey(t: Template): [string, any] {
  const entries = Object.entries(t.findResources("AWS::KMS::Key"));
  expect(entries).toHaveLength(1);
  return entries[0] as [string, any];
}

function keyStatements(t: Template): any[] {
  return datasetKey(t)[1].Properties.KeyPolicy.Statement;
}

function keyStatement(t: Template, sid: string): any {
  return keyStatements(t).find((s) => s.Sid === sid);
}

function actionsOf(statement: any): string[] {
  const action = statement.Action;
  return Array.isArray(action) ? action : [action];
}

describe("hot cache bucket", () => {
  const t = synth();

  it("encrypts with the dataset key and nothing else", () => {
    const [keyId] = datasetKey(t);
    const sse =
      cacheBucket(t)[1].Properties.BucketEncryption.ServerSideEncryptionConfiguration[0];

    expect(sse.ServerSideEncryptionByDefault.SSEAlgorithm).toBe("aws:kms");
    expect(sse.ServerSideEncryptionByDefault.KMSMasterKeyID).toEqual({
      "Fn::GetAtt": [keyId, "Arn"],
    });
  });

  it("keeps bucket keys off so every read re-evaluates the key policy", () => {
    const sse =
      cacheBucket(t)[1].Properties.BucketEncryption.ServerSideEncryptionConfiguration[0];

    // Explicitly false, not merely absent: with bucket keys on, S3 caches a
    // bucket-level key and later reads neither call KMS nor re-check the key
    // policy, which would leave a disabled or pending-deletion key still
    // yielding readable objects.
    expect(sse.BucketKeyEnabled).toBe(false);
  });

  it("retains the bucket and the key rather than deleting either", () => {
    const retained = [
      ...Object.values(t.findResources("AWS::S3::Bucket")),
      ...Object.values(t.findResources("AWS::KMS::Key")),
    ];
    expect(retained).toHaveLength(3);

    for (const res of retained) {
      expect((res as any).DeletionPolicy).toBe("Retain");
      expect((res as any).UpdateReplacePolicy).toBe("Retain");
    }
  });

  it("blocks public access and takes ownership of every object", () => {
    const props = cacheBucket(t)[1].Properties;

    expect(props.PublicAccessBlockConfiguration).toEqual({
      BlockPublicAcls: true,
      BlockPublicPolicy: true,
      IgnorePublicAcls: true,
      RestrictPublicBuckets: true,
    });
    expect(props.OwnershipControls.Rules).toEqual([
      { ObjectOwnership: "BucketOwnerEnforced" },
    ]);
  });

  it("carries an expiry backstop set far beyond any release", () => {
    const rules = cacheBucket(t)[1].Properties.LifecycleConfiguration.Rules;
    const backstop = rules.find((r: any) => r.Id === "ExpireCachedData");

    expect(backstop).toBeDefined();
    expect(backstop.Status).toBe("Enabled");
    // Access stops via a time condition in the bucket and key policies, so this
    // rule may only ever be a storage-cost floor — never the expiry mechanism,
    // which is why it has to sit well past any plausible release.
    expect(backstop.ExpirationInDays).toBe(365);
    expect(backstop.ExpirationInDays).toBeGreaterThanOrEqual(90);

    const multipart = rules.find(
      (r: any) => r.Id === "CleanupIncompleteMultipartUploads",
    );
    expect(multipart.AbortIncompleteMultipartUpload).toEqual({ DaysAfterInitiation: 7 });
  });

  it("honours a longer retention backstop and refuses one below the floor", () => {
    const longer = synth({ dataRetentionDays: 900 });
    const rules = cacheBucket(longer)[1].Properties.LifecycleConfiguration.Rules;
    expect(rules.find((r: any) => r.Id === "ExpireCachedData").ExpirationInDays).toBe(900);

    expect(() => buildStack({ dataRetentionDays: 30 })).toThrow(/at least 90/);
  });

  it("deploys no grant, leaving only the insecure-transport deny", () => {
    const [bucketId] = cacheBucket(t);
    const policies = Object.values(t.findResources("AWS::S3::BucketPolicy")).filter(
      (p) => (p as any).Properties.Bucket?.Ref === bucketId,
    );
    expect(policies).toHaveLength(1);

    const statements = (policies[0] as any).Properties.PolicyDocument.Statement;
    expect(statements).toHaveLength(1);
    expect(statements[0]).toMatchObject({
      Effect: "Deny",
      Action: "s3:*",
      Principal: { AWS: "*" },
      Condition: { Bool: { "aws:SecureTransport": "false" } },
    });
    // Researcher grants are written by the releaser when a release is made, not
    // laid down at deploy time.
    expect(statements.filter((s: any) => s.Effect === "Allow")).toHaveLength(0);
  });

  it("reserves a prefix for copy artefacts and reports it", () => {
    t.hasOutput("ControlPrefix", { Value: `${CONTROL_PREFIX}/` });
    t.hasOutput("HotCacheBucketName", {});
    t.hasOutput("DatasetKeyArn", {});
    t.hasOutput("DatasetKeyId", {});
  });
});

describe("dataset key policy", () => {
  const t = synth();

  it("leaves key administration with the owning account", () => {
    const admin = keyStatements(t).find(
      (s) => actionsOf(s).includes("kms:*") && s.Effect === "Allow",
    );

    expect(admin).toBeDefined();
    expect(JSON.stringify(admin.Principal)).toContain(`:iam::${ACCOUNT}:root`);
  });

  it("names the copy writer role, scoped to this bucket's objects", () => {
    const bucketName = cacheBucket(t)[1].Properties.BucketName;
    const writer = keyStatement(t, "AllowCopyWriterToEncrypt");

    expect(writer).toBeDefined();
    expect(writer.Effect).toBe("Allow");
    expect(writer.Principal).toEqual({
      AWS: `arn:aws:iam::${ACCOUNT}:role/${COPY_WRITER_ROLE}`,
    });
    expect(actionsOf(writer)).toEqual([
      "kms:GenerateDataKey",
      "kms:GenerateDataKeyWithoutPlaintext",
      "kms:Decrypt",
      "kms:DescribeKey",
    ]);
    expect(writer.Condition).toEqual({
      StringLike: {
        "kms:EncryptionContext:aws:s3:arn": `arn:aws:s3:::${bucketName}/*`,
      },
    });
  });

  it("points the copy writer grant at the role's own account when it runs elsewhere", () => {
    const elsewhere = synth({ copyWriterRoleAccount: "444444444444" });
    const writer = keyStatement(elsewhere, "AllowCopyWriterToEncrypt");

    expect(writer.Principal).toEqual({
      AWS: `arn:aws:iam::444444444444:role/${COPY_WRITER_ROLE}`,
    });
  });

  it("gives the custodian the kill switch but no way to take it back", () => {
    const custodian = keyStatement(t, "AllowCustodianToWithdraw");

    expect(custodian).toBeDefined();
    expect(custodian.Effect).toBe("Allow");
    expect(custodian.Principal).toEqual({ AWS: CUSTODIAN_ARN });
    expect(actionsOf(custodian)).toEqual([
      "kms:DisableKey",
      "kms:ScheduleKeyDeletion",
      "kms:DescribeKey",
    ]);
    // Deliberate: undoing a scheduled deletion belongs to the account's key
    // administrators, not to the custodian alone.
    expect(JSON.stringify(keyStatements(t))).not.toContain("kms:CancelKeyDeletion");
  });

  it("omits the custodian statement when no custodian is configured", () => {
    const withoutCustodian = synth({ custodianRoleArn: undefined });

    expect(keyStatement(withoutCustodian, "AllowCustodianToWithdraw")).toBeUndefined();
    expect(JSON.stringify(keyStatements(withoutCustodian))).not.toContain(
      "kms:ScheduleKeyDeletion",
    );
  });

  it("names at least one principal that can actually decrypt", () => {
    const canDecrypt = keyStatements(t).filter(
      (s) =>
        s.Effect === "Allow" &&
        actionsOf(s).some((a) => a === "kms:Decrypt" || a === "kms:*"),
    );

    expect(canDecrypt.length).toBeGreaterThan(0);
  });

  it("grants no principal outside the owning account at deploy time", () => {
    const foreign = keyStatements(t).filter((s) => {
      const principal = JSON.stringify(s.Principal ?? "");
      return s.Effect === "Allow" && !principal.includes(ACCOUNT);
    });

    expect(foreign).toEqual([]);
  });

  it("rotates the key and keeps a recovery window on scheduled deletion", () => {
    const props = datasetKey(t)[1].Properties;
    expect(props.EnableKeyRotation).toBe(true);
    expect(props.PendingWindowInDays).toBe(7);

    const longerWindow = synth({ keyPendingWindowDays: 30 });
    expect(datasetKey(longerWindow)[1].Properties.PendingWindowInDays).toBe(30);
  });

  it("aliases the key after the bucket it encrypts", () => {
    const bucketName = cacheBucket(t)[1].Properties.BucketName;
    t.hasResourceProperties("AWS::KMS::Alias", { AliasName: `alias/${bucketName}` });
  });
});

describe("object access logging", () => {
  it("logs to a separate bucket the researcher cannot read", () => {
    const t = synth();
    const [cacheId, cache] = cacheBucket(t);
    const logs = logBucket(t);

    expect(cache.Properties.LoggingConfiguration.DestinationBucketName.Ref).not.toBe(
      cacheId,
    );
    expect(logs.Properties.BucketName).toBe(`${cache.Properties.BucketName}-logs`);
  });

  it("encrypts the log bucket with S3-managed keys, so a purge cannot erase the record", () => {
    const t = synth();
    const [keyId] = datasetKey(t);
    const encryption = logBucket(t).Properties.BucketEncryption;

    expect(
      encryption.ServerSideEncryptionConfiguration[0].ServerSideEncryptionByDefault
        .SSEAlgorithm,
    ).toBe("AES256");
    expect(JSON.stringify(encryption)).not.toContain(keyId);
  });

  it("creates only the cache bucket when logging is switched off", () => {
    const t = synth({ logObjectAccess: false });

    t.resourceCountIs("AWS::S3::Bucket", 1);
    expect(cacheBucket(t)[1].Properties.LoggingConfiguration).toBeUndefined();
  });

  it("uses an existing log bucket when one is named", () => {
    const t = synth({ accessLogsBucketName: "central-access-logs" });

    t.resourceCountIs("AWS::S3::Bucket", 1);
    expect(cacheBucket(t)[1].Properties.LoggingConfiguration.DestinationBucketName).toBe(
      "central-access-logs",
    );
  });

  it("refuses a bucket name too long to derive a log bucket name from", () => {
    const tooLong = "x".repeat(MAX_DERIVED_NAME + 1);

    expect(() => buildStack({ bucketName: tooLong })).toThrow(/too long/);
    // Both escape hatches the message offers have to work.
    expect(() =>
      buildStack({ bucketName: tooLong, logObjectAccess: false }),
    ).not.toThrow();
    expect(() =>
      buildStack({ bucketName: tooLong, accessLogsBucketName: "central-access-logs" }),
    ).not.toThrow();
  });
});

describe("dataset labelling", () => {
  it("tags the bucket and key with the dataset URI when names are legible", () => {
    const t = synth();

    for (const res of [cacheBucket(t)[1], datasetKey(t)[1]]) {
      expect(res.Properties.Tags).toEqual(
        expect.arrayContaining([{ Key: "DatasetUri", Value: DATASET_URI }]),
      );
    }
  });

  it("omits the tag when the name is meant to obscure the dataset", () => {
    const t = synth({ bucketNameHashKey: "hash-key" });

    expect(JSON.stringify(t.toJSON())).not.toContain(DATASET_URI);
    for (const res of [cacheBucket(t)[1], datasetKey(t)[1]]) {
      expect(res.Properties.Tags).toBeUndefined();
    }
  });

  it("requires a dataset URI", () => {
    expect(() => buildStack({ datasetUri: "   " })).toThrow(/datasetUri is required/);
  });
});

describe("bucket name derivation", () => {
  const base = {
    namePrefix: NAME_PREFIX,
    datasetUri: DATASET_URI,
    account: ACCOUNT,
    region: REGION,
  };

  it("is legible and stable without a hash key", () => {
    const name = hotCacheBucketName(base);

    expect(name).toBe(hotCacheBucketName(base));
    expect(name).toContain("ceph-source-datasets-demo");
    expect(name.startsWith(`${NAME_PREFIX}-dataset-`)).toBe(true);
  });

  it("reveals nothing about the dataset with a hash key", () => {
    const name = hotCacheBucketName({ ...base, hashKey: "hash-key" });

    expect(name).toBe(hotCacheBucketName({ ...base, hashKey: "hash-key" }));
    expect(name).toMatch(
      new RegExp(`^${NAME_PREFIX}-dataset-[0-9a-f]{16}$`),
    );
    expect(name).not.toContain("ceph");
    expect(name).not.toContain("demo");
    // A different key over the same URI has to land somewhere else, or the
    // digest is not keyed at all.
    expect(name).not.toBe(hotCacheBucketName({ ...base, hashKey: "other-key" }));
  });

  it("scopes the hashed name to the account and region", () => {
    const here = hotCacheBucketName({ ...base, hashKey: "hash-key" });

    expect(
      hotCacheBucketName({ ...base, account: "111111111111", hashKey: "hash-key" }),
    ).not.toBe(here);
    expect(
      hotCacheBucketName({ ...base, region: "us-east-1", hashKey: "hash-key" }),
    ).not.toBe(here);
  });

  it("stays a legal bucket name however long the dataset URI is", () => {
    for (const uri of [
      DATASET_URI,
      `s3://ceph-source/${"a".repeat(300)}`,
      "s3://CEPH_Source/Datasets/Demo Set..1/",
    ]) {
      for (const hashKey of [undefined, "hash-key"]) {
        const name = hotCacheBucketName({ ...base, datasetUri: uri, hashKey });

        expect(name.length).toBeLessThanOrEqual(MAX_DERIVED_NAME);
        expect(name).toMatch(/^[a-z0-9][a-z0-9-]+[a-z0-9]$/);
      }
    }
  });

  it("still yields a name when the URI slugifies to nothing", () => {
    expect(hotCacheBucketName({ ...base, datasetUri: "s3://" })).toContain("unnamed");
  });

  it("keeps distinct datasets on distinct names even when their slugs collide", () => {
    const a = hotCacheBucketName({
      ...base,
      datasetUri: `s3://ceph-source/${"a".repeat(300)}/one`,
    });
    const b = hotCacheBucketName({
      ...base,
      datasetUri: `s3://ceph-source/${"a".repeat(300)}/two`,
    });

    expect(a).not.toBe(b);
  });

  it("uses an explicit bucket name verbatim", () => {
    const t = synth({ bucketName: "pinned-hot-cache-bucket" });

    expect(cacheBucket(t)[1].Properties.BucketName).toBe("pinned-hot-cache-bucket");
  });
});

describe("one stack per dataset", () => {
  const DATASETS: DatasetConfig[] = [
    { name: "Demo", datasetUri: "s3://ceph-source/datasets/demo" },
    { name: "Cohort-2024", datasetUri: "s3://ceph-source/datasets/cohort-2024" },
  ];

  function stacksFor(
    overrides: Partial<HotCacheStacksOptions> = {},
  ): HotCacheStack[] {
    return hotCacheStacks(new cdk.App(), {
      env: { account: ACCOUNT, region: REGION },
      namePrefix: NAME_PREFIX,
      copyWriterRoleName: COPY_WRITER_ROLE,
      datasets: DATASETS,
      ...overrides,
    });
  }

  it("names each stack after its dataset", () => {
    const stacks = stacksFor();

    expect(stacks.map((s) => s.stackName)).toEqual([
      hotCacheStackName("Demo"),
      hotCacheStackName("Cohort-2024"),
    ]);
  });

  it("gives every dataset a key of its own, so destroying one spares the rest", () => {
    for (const stack of stacksFor()) {
      const t = Template.fromStack(stack);
      t.resourceCountIs("AWS::KMS::Key", 1);
      expect(cacheBucket(t)[1].Properties.BucketName).toBeDefined();
    }

    const names = stacksFor().map((s) => s.bucketName);
    expect(new Set(names).size).toBe(names.length);
  });

  it("deploys nothing, and demands nothing, when no dataset is configured", () => {
    expect(
      hotCacheStacks(new cdk.App(), {
        env: { account: ACCOUNT, region: REGION },
        namePrefix: NAME_PREFIX,
        datasets: [],
      }),
    ).toEqual([]);
  });

  it("refuses to synthesise without the copy writer role name", () => {
    expect(() => stacksFor({ copyWriterRoleName: undefined })).toThrow(
      /copyWriterRoleName is required/,
    );
  });

  it("refuses a dataset name that cannot be a stack name", () => {
    for (const name of ["1demo", "demo_set", "-demo", "demo set", ""]) {
      expect(() =>
        stacksFor({ datasets: [{ name, datasetUri: DATASET_URI }] }),
      ).toThrow(/must start with a letter/);
    }
  });

  it("refuses a dataset with no URI", () => {
    expect(() =>
      stacksFor({ datasets: [{ name: "Demo", datasetUri: "  " }] }),
    ).toThrow(/missing datasetUri/);
  });

  it("refuses the same dataset name twice", () => {
    expect(() =>
      stacksFor({
        datasets: [
          { name: "Demo", datasetUri: "s3://ceph-source/datasets/one" },
          { name: "Demo", datasetUri: "s3://ceph-source/datasets/two" },
        ],
      }),
    ).toThrow(/configured twice; each names one stack/);
  });

  it("refuses two names for one dataset URI, which would contend for one bucket", () => {
    expect(() =>
      stacksFor({
        datasets: [
          { name: "Demo", datasetUri: DATASET_URI },
          { name: "DemoAgain", datasetUri: DATASET_URI },
        ],
      }),
    ).toThrow(/configured twice, under DemoAgain/);
  });

  it("lets one dataset override the account-wide retention backstop", () => {
    const stacks = stacksFor({
      dataRetentionDays: 400,
      datasets: [
        { name: "Demo", datasetUri: "s3://ceph-source/datasets/one" },
        {
          name: "Cohort",
          datasetUri: "s3://ceph-source/datasets/two",
          dataRetentionDays: 1000,
        },
      ],
    });

    const expiries = stacks.map(
      (s) =>
        cacheBucket(Template.fromStack(s))[1].Properties.LifecycleConfiguration.Rules.find(
          (r: any) => r.Id === "ExpireCachedData",
        ).ExpirationInDays,
    );
    expect(expiries).toEqual([400, 1000]);
  });

  it("passes the custodian and copy writer through to each dataset's key", () => {
    const stacks = stacksFor({ custodianRoleArn: CUSTODIAN_ARN });

    for (const stack of stacks) {
      const t = Template.fromStack(stack);
      expect(keyStatement(t, "AllowCopyWriterToEncrypt")).toBeDefined();
      expect(keyStatement(t, "AllowCustodianToWithdraw").Principal).toEqual({
        AWS: CUSTODIAN_ARN,
      });
    }
  });
});

describe("bucket name hash key resolution", () => {
  it("is not needed when names are legible", () => {
    expect(resolveBucketNameHashKey(false, {})).toBeUndefined();
    expect(
      resolveBucketNameHashKey(false, { [BUCKET_NAME_HASH_KEY_VAR]: "ignored" }),
    ).toBeUndefined();
  });

  it("comes from the environment, never from checked-in context", () => {
    expect(
      resolveBucketNameHashKey(true, { [BUCKET_NAME_HASH_KEY_VAR]: "hash-key" }),
    ).toBe("hash-key");
  });

  it("refuses to fall back to a legible name, which would abandon the deployed bucket", () => {
    expect(() => resolveBucketNameHashKey(true, {})).toThrow(
      new RegExp(BUCKET_NAME_HASH_KEY_VAR),
    );
    expect(() =>
      resolveBucketNameHashKey(true, { [BUCKET_NAME_HASH_KEY_VAR]: "" }),
    ).toThrow(new RegExp(BUCKET_NAME_HASH_KEY_VAR));
  });
});
