/**
 * Canonical test-data fixtures, shared by the stack that hosts the objects
 * (DataShareTestStack) and the mock warehouse that advertises them
 * (MockMartStack). Keeping the name and the object list in one place is what
 * guarantees every library the mock Mart resolves has a real object behind it in
 * the bucket — the property an end-to-end / integration test depends on.
 */

/** One object seeded into the test bucket and advertised by the mock Mart. */
export interface TestDataObject {
  /** Library ID the CLI is invoked with (`--libraries`) and the Mart row key. */
  readonly libraryId: string;
  /**
   * S3 key of the BAM under the test bucket (no leading slash). A vendored file
   * must sit at exactly this path under infrastructure/test-data/, with its
   * `.bam.bai` index beside it, so that BucketDeployment reproduces these keys
   * verbatim. Synth fails fast when one is missing.
   */
  readonly key: string;
}

/**
 * The objects the test bucket is populated with, sourced from the htsget-rs test
 * data repository (umccr/htsget-rs, `data/bam`). One entry per shareable read;
 * the co-located `.bam.bai` index is uploaded automatically because it shares
 * the key prefix, and htsget-rs needs it for range queries.
 *
 * This is the single source of truth: the fixtures are vendored at these keys
 * under test-data/, DataShareTestStack uploads them, and MockMartStack defaults
 * its rows to them so the mock warehouse and the bucket never drift apart.
 */
export const TEST_DATA_OBJECTS: TestDataObject[] = [
  { libraryId: "L2400001", key: "demo/L2400001/NA12878.bam" },
];

/**
 * Physical name of the test data bucket: `data-share-test-<account>-<region>`.
 *
 * Account- and region-qualified for two reasons: the name is then unique in the
 * global S3 namespace, so it cannot be squatted or accidentally resolved in
 * another account; and one source tree can deploy to several environments
 * without a bucket-name collision.
 */
export function dataShareTestBucketName(account: string, region: string): string {
  return `data-share-test-${account}-${region}`;
}
