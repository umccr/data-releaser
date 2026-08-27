import { CfnOutput, RemovalPolicy, Stack, StackProps } from "aws-cdk-lib";
import * as s3 from "aws-cdk-lib/aws-s3";
import * as s3deploy from "aws-cdk-lib/aws-s3-deployment";
import { Construct } from "constructs";
import { dataShareTestBucketName } from "./test-data";

export interface DataShareTestStackProps extends StackProps {
  /**
   * Prefix applied to logical construct ids.
   */
  readonly namePrefix: string;

  /**
   * Local directory whose tree is uploaded to the bucket.
   */
  readonly sourcePath: string;
}

/**
 * Owns the mock test-data bucket, `data-share-test-<account>-<region>`.
 */
export class DataShareTestStack extends Stack {
  readonly bucketName: string;

  constructor(scope: Construct, id: string, props: DataShareTestStackProps) {
    super(scope, id, props);

    const bucket = new s3.Bucket(this, "DataShareTestBucket", {
      bucketName: dataShareTestBucketName(this.account, this.region),
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      enforceSSL: true,
      removalPolicy: RemovalPolicy.DESTROY,
      autoDeleteObjects: true,
    });

    new s3deploy.BucketDeployment(this, "DataShareTestObjects", {
      destinationBucket: bucket,
      sources: [s3deploy.Source.asset(props.sourcePath)],
      prune: true,
    });

    this.bucketName = bucket.bucketName;

    new CfnOutput(this, "DataShareTestBucketName", {
      value: bucket.bucketName,
      description:
        "Test data bucket for data sharer."
    });
  }
}
