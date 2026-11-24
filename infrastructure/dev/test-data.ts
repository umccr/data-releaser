/**
 * The test data fixtures.
 */

/**
 * An object inside the mock Mart.
 */
export interface TestDataObject {
  /**
   * Library ID the CLI.
   */
  readonly libraryId: string;
  /**
   * S3 key of the BAM under the test bucket.
   */
  readonly key: string;
}

/**
 * The objects the test bucket is populated with.
 */
export const TEST_DATA_OBJECTS: TestDataObject[] = [
  { libraryId: "L2400001", key: "demo/L2400001/NA12878.bam" },
];

/**
 * Name of the test data bucket.
 */
export function dataShareTestBucketName(account: string, region: string): string {
  return `data-share-test-${account}-${region}`;
}
