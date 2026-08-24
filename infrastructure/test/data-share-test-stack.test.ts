import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import * as cdk from "aws-cdk-lib";
import { Template } from "aws-cdk-lib/assertions";
import { DataShareTestStack } from "../lib/data-share-test-stack";
import { dataShareTestBucketName, TEST_DATA_OBJECTS } from "../lib/test-data";

const ACCOUNT = "000000000000";
const REGION = "ap-southeast-2";

function makeSourceTree(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "data-share-test-"));
  for (const obj of TEST_DATA_OBJECTS) {
    const file = path.join(dir, obj.key);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, "test-fixture");
    fs.writeFileSync(`${file}.bai`, "test-fixture-index");
  }
  return dir;
}

function synth(): Template {
  const app = new cdk.App();
  const stack = new DataShareTestStack(app, "TestDataShare", {
    env: { account: ACCOUNT, region: REGION },
    namePrefix: "data-releaser",
    sourcePath: makeSourceTree(),
  });
  return Template.fromStack(stack);
}

describe("Data share test bucket", () => {
  const t = synth();

  it("bucket naming", () => {
    t.hasResourceProperties("AWS::S3::Bucket", {
      BucketName: dataShareTestBucketName(ACCOUNT, REGION),
    });
  });

  it("blocks all public access", () => {
    t.hasResourceProperties("AWS::S3::Bucket", {
      PublicAccessBlockConfiguration: {
        BlockPublicAcls: true,
        BlockPublicPolicy: true,
        IgnorePublicAcls: true,
        RestrictPublicBuckets: true,
      },
    });
  });

  it("is disposable", () => {
    t.hasResource("AWS::S3::Bucket", { DeletionPolicy: "Delete" });
  });

  it("refuses non-TLS requests and never grants a public principal", () => {
    const policies = t.findResources("AWS::S3::BucketPolicy");
    const keys = Object.keys(policies);
    expect(keys).toHaveLength(1);

    const statements: Array<{ Effect: string; Principal?: unknown; Condition?: unknown }> =
      policies[keys[0]].Properties.PolicyDocument.Statement;

    expect(
      statements.some(
        (s) =>
          s.Effect === "Deny" &&
          JSON.stringify(s.Condition ?? {}).includes("aws:SecureTransport"),
      ),
    ).toBe(true);

    for (const s of statements) {
      if (s.Effect !== "Allow") continue;
      expect(JSON.stringify(s.Principal ?? {})).not.toContain('"*"');
    }
  });

  it("uploads the fixture to the bucket", () => {
    t.resourceCountIs("Custom::CDKBucketDeployment", 1);
  });
});
