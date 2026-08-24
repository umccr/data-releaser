import { CfnOutput, RemovalPolicy, Stack, StackProps } from "aws-cdk-lib";
import * as s3 from "aws-cdk-lib/aws-s3";
import * as s3deploy from "aws-cdk-lib/aws-s3-deployment";
import { Construct } from "constructs";
import { dataShareTestBucketName } from "./test-data";

export interface DataShareTestStackProps extends StackProps {
  /** Prefix applied to logical construct ids (parity with the other stacks). */
  readonly namePrefix: string;

  /**
   * Local directory whose tree is uploaded verbatim to the bucket. Its layout IS
   * the bucket's key layout, so a file at
   * `<sourcePath>/demo/L2400001/NA12878.bam` lands at
   * `s3://<bucket>/demo/L2400001/NA12878.bam`. Populated from the htsget-rs test
   * data repo by scripts/fetch-test-data.sh (run `pnpm run fetch:test-data`).
   */
  readonly sourcePath: string;
}

/**
 * Owns the mock test-data bucket, `data-share-test-<account>-<region>`.
 *
 * In the real system the shareable reads live in a Data_Account bucket outside
 * this repo. For the mock/demo set-up there is no such account, so this stack
 * stands one up: a locked-down bucket seeded with the public htsget-rs sample
 * BAM, which htsget-rs serves and the mock Mart (MockMartStack) advertises.
 * Eventually it is the fixture an end-to-end integration test resolves against.
 *
 * The bucket holds only the public htsget-rs sample data — no genomic PHI — and
 * is entirely reproducible from the fetch script, so it is disposable: it and
 * its contents are destroyed with the stack. It is emphatically NOT a production
 * data location.
 *
 * Optional and off by default; deployed only when the `dataShareTest` context
 * flag is set (see bin/data-releaser.ts).
 */
export class DataShareTestStack extends Stack {
  /** Physical bucket name, for wiring htsget-rs and the mock Mart to it. */
  readonly bucketName: string;

  constructor(scope: Construct, id: string, props: DataShareTestStackProps) {
    super(scope, id, props);

    // No accidental public exposure: block every public-access vector, refuse
    // non-TLS requests, and disable ACLs (BUCKET_OWNER_ENFORCED) so no object
    // writer can hand ownership to another account. Account/region-qualified
    // name so it cannot be squatted in the global S3 namespace.
    const bucket = new s3.Bucket(this, "DataShareTestBucket", {
      bucketName: dataShareTestBucketName(this.account, this.region),
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
    });

    // Upload the fixture tree verbatim: the local directory layout becomes the
    // bucket key layout, so htsget resolves an id like
    // `/reads/demo/L2400001/NA12878` against `demo/L2400001/NA12878.bam` here.
    // prune:true keeps the bucket an exact mirror of the fixtures, dropping
    // anything a previous deploy left behind.
    new s3deploy.BucketDeployment(this, "DataShareTestObjects", {
      destinationBucket: bucket,
      sources: [s3deploy.Source.asset(props.sourcePath)],
      prune: true,
    });

    this.bucketName = bucket.bucketName;

    new CfnOutput(this, "DataShareTestBucketName", {
      value: bucket.bucketName,
      description:
        "Test data bucket. Use as the htsget-rs static location and as the mock " +
        "Mart rows' bucket column so shares resolve to real objects.",
    });
  }
}
