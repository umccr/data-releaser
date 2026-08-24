import * as cdk from "aws-cdk-lib";
import { Template, Match } from "aws-cdk-lib/assertions";
import { MockMartObject, MockMartStack } from "../lib/mock-mart-stack";

const DEFAULT_OBJECTS: MockMartObject[] = [
  { libraryId: "L2400001", key: "demo/L2400001/NA12878.bam" },
  { libraryId: "L2400002", key: "demo/L2400002/NA12879.bam" },
];

function synth(objects: MockMartObject[] = DEFAULT_OBJECTS): Template {
  const app = new cdk.App();
  const stack = new MockMartStack(app, "TestMockMart", {
    env: { account: "000000000000", region: "ap-southeast-2" },
    namePrefix: "data-releaser",
    databaseName: "mart_mock",
    workgroupName: "data-releaser-mock",
    dataBucketName: "data-share-test-000000000000-ap-southeast-2",
    cohortId: "DEMO",
    objects,
  });
  return Template.fromStack(stack);
}

describe("Mock Mart Glue catalogue", () => {
  const t = synth();

  it("creates the database the resolution query names", () => {
    t.hasResourceProperties("AWS::Glue::Database", {
      DatabaseInput: { Name: "mart_mock" },
    });
  });

  it("exposes every column the resolution query selects", () => {
    const tables = t.findResources("AWS::Glue::Table");
    const keys = Object.keys(tables);
    expect(keys).toHaveLength(1);

    const tableInput = tables[keys[0]].Properties.TableInput;
    expect(tableInput.Name).toBe("bam");

    const columns: string[] = tableInput.StorageDescriptor.Columns.map(
      (c: { Name: string }) => c.Name,
    );
    for (const required of [
      "library_id",
      "portal_run_id",
      "bucket",
      "key",
      "format",
      "storage_class",
    ]) {
      expect(columns).toContain(required);
    }
  });

  it("reads JSON lines", () => {
    t.hasResourceProperties("AWS::Glue::Table", {
      TableInput: {
        TableType: "EXTERNAL_TABLE",
        StorageDescriptor: {
          SerdeInfo: {
            SerializationLibrary: "org.openx.data.jsonserde.JsonSerDe",
          },
        },
      },
    });
  });
});

describe("Mock Mart Athena workgroup", () => {
  const t = synth();

  it("supplies and enforces a result output location", () => {
    t.hasResourceProperties("AWS::Athena::WorkGroup", {
      Name: "data-releaser-mock",
      WorkGroupConfiguration: {
        EnforceWorkGroupConfiguration: true,
        ResultConfiguration: {
          OutputLocation: Match.anyValue(),
        },
      },
    });

    const groups = t.findResources("AWS::Athena::WorkGroup");
    const config = groups[Object.keys(groups)[0]].Properties.WorkGroupConfiguration;
    const location = JSON.stringify(config.ResultConfiguration.OutputLocation);
    expect(location).toContain("s3://");
    expect(location).toContain("/athena-results/");
  });
});

describe("Mock Mart rows", () => {
  it("are deterministic across synths", () => {
    const a = JSON.stringify(synth().toJSON().Resources);
    const b = JSON.stringify(synth().toJSON().Resources);
    expect(a).toBe(b);
  });

  it("stores the table data and the query results in one disposable bucket", () => {
    const t = synth();
    t.hasResourceProperties("AWS::S3::Bucket", {
      PublicAccessBlockConfiguration: {
        BlockPublicAcls: true,
        BlockPublicPolicy: true,
        IgnorePublicAcls: true,
        RestrictPublicBuckets: true,
      },
    });
    t.hasResource("AWS::S3::Bucket", { DeletionPolicy: "Delete" });
  });
});
