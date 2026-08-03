import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import * as cdk from "aws-cdk-lib";
import { Template } from "aws-cdk-lib/assertions";
import { DataShareTestStack } from "../lib/data-share-test-stack";
import { dataShareTestBucketName, TEST_DATA_OBJECTS } from "../lib/test-data";

/**
 * Assertions over the mock test-data bucket. The guarantee being pinned here is
 * the one the security review turned on: this bucket cannot leak its contents
 * publicly, and it is disposable rather than a production data location.
 */

const ACCOUNT = "000000000000";
const REGION = "ap-southeast-2";

/**
 * A throwaway source tree matching TEST_DATA_OBJECTS. BucketDeployment stages the
 * directory at synth time, so the files must actually exist — their contents are
 * irrelevant to the template assertions.
 */
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

  it("names the bucket data-share-test-<account>-<region>", () => {
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

  it("is disposable — destroyed with the stack", () => {
    t.hasResource("AWS::S3::Bucket", { DeletionPolicy: "Delete" });
  });

  it("refuses non-TLS requests and never grants a public principal", () => {
    // enforceSSL adds a Deny for aws:SecureTransport=false. The only Allow the
    // policy may carry is the autoDeleteObjects cleanup role (a named AWS
    // principal) — never a wildcard principal, which is the public-read grant
    // this bucket exists to prove impossible.
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

  it("uploads the fixture tree to the bucket", () => {
    // The BucketDeployment custom resource is what puts the objects there.
    t.resourceCountIs("Custom::CDKBucketDeployment", 1);
  });
});
