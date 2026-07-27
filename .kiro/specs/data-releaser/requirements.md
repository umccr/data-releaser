# Requirements Document

## Introduction

The Data Releaser is a CLI tool that enables UMCCR administrators to share bioinformatics data (identified by `library_id`) with external researchers via the htsget-rs protocol. The tool coordinates data sharing across multiple AWS accounts by managing an authorization backend that htsget-rs queries to determine access permissions, and by maintaining VPC Lattice connectivity to researcher accounts. Researchers consume shared data through Seqera via a VPC Lattice connection to an htsget-rs endpoint in the data-control account.

The tool resolves library IDs to S3 file locations by querying the downstream OrcaVault data warehouse (its curated `mart` layer) via Athena over a Lake Formation cross-account share, rather than the upstream OrcaBus service APIs. This makes the warehouse the single source of the `library_id -> portal_run_id -> S3 path` mapping. The current version shares aligned-read BAM files only (BAM/BAI); variant (VCF/BCF) sharing is deferred until a corresponding warehouse mart exists.

This project also owns the deployment of the Data_Control_Account resources the tool depends on to operate — the Authorization_Service Lambda functions, the Share_Record DynamoDB table, and the API Gateway in front of them — delivered as an AWS CDK application (Requirement 16). It does not deploy htsget-rs, OrcaHouse/OrcaVault, the cross-account Lake Formation or VPC Lattice sharing arrangements, or any resource in a Researcher_Account; what it assumes those other projects provide is stated explicitly in Requirement 17.

## Glossary

- **Data_Releaser**: The Go CLI tool that administrators use to initiate, manage, and revoke data shares. It runs under a single authenticated session into the Data_Control_Account (resolved via the standard AWS credential chain) and uses that identity for every command.
- **Authorization_Service**: A lightweight backend service deployed in the data-control account that stores share records and responds to htsget-rs authorization callouts with access rules.
- **Authorization_Cache**: The combined caching layer for authorization decisions, comprising (a) an API Gateway response cache in front of the Authorization_Service and (b) an in-execution-environment cache within the Htsget_Rs Lambda. Both layers are keyed on the requester identity so decisions remain isolated per Researcher_Account.
- **Cache_TTL**: The maximum time a cached authorization response may be reused before a fresh callout is required. It is driven by the `Cache-Control: max-age` emitted by the Authorization_Service and capped at a configurable ceiling. The Cache_TTL bounds how long a revocation or expiry may take to propagate.
- **Share_Record**: A persistent record describing which library IDs are shared to which AWS account, including expiry and status.
- **OrcaHouse**: The UMCCR Enterprise Data Warehouse project. Its first warehouse, OrcaVault, consolidates data from the upstream OrcaBus microservices (metadata-manager, workflow-manager, filemanager, sequence-run-manager) for analytics and audit.
- **OrcaVault**: The OrcaHouse data warehouse (dbt on Redshift Serverless, Data Vault 2.0) deployed in the Warehouse_Account. It ingests OrcaBus service databases via DMS change-data-capture into an S3 datalake and rebuilds curated tables on a daily schedule. It is the single source the Data_Releaser uses to resolve library IDs to S3 file locations. Because it is a downstream, batch-refreshed warehouse, resolution reflects the state of the most recent daily build and may lag live OrcaBus state by up to roughly a day; this is acceptable for sharing already-produced data.
- **Mart**: The curated, business-facing layer of OrcaVault (Glue database `mart`) that the Data_Releaser queries. Relevant tables:
  - `mart.bam`: one row per aligned-read file AND per index file, with `library_id`, `portal_run_id`, `bucket`, `key`, `filename`, `format` (`bam` and `bai`; no `cram`), `size`, `storage_class`, and `last_modified_date`. Already curated to the current, non-deleted, effective S3 location. It is the single source that determines whether a `library_id` is shareable.
  - `mart.workflow`: maps `portal_run_id` to workflow name/status/version and `library_id`.
- **Athena**: The AWS query service used to run read-only SQL against the OrcaVault Mart. The Data_Releaser resolves libraries by issuing an Athena query (`StartQueryExecution`), polling for completion, and reading results from the workgroup's S3 output location.
- **LakeFormation_Share**: The AWS Lake Formation cross-account sharing mechanism by which the Warehouse_Account shares the OrcaVault Mart (Glue catalog tables, governed via AWS RAM) to consumer accounts. The Data_Control_Account is a consumer: it holds a Lake Formation resource-link database (`mart`) pointing at the shared warehouse database, and local principals are granted `SELECT`/`DESCRIBE`. This lets the Data_Releaser query the Mart from within the Data_Control_Account without holding any credentials in the Warehouse_Account.
- **Warehouse_Account**: AWS account (115253169271, `ap-southeast-2`) that hosts OrcaHouse/OrcaVault and owns the Mart shared via Lake Formation. The Data_Releaser does not authenticate into this account directly; it consumes the Mart through the Lake Formation share in the Data_Control_Account.
- **Htsget_Rs**: The htsget-rs server deployed in the data-control account as an AWS Lambda, serving bioinformatics data via the GA4GH htsget protocol.
- **VPC_Lattice_Share**: An AWS VPC Lattice service network share via AWS RAM that allows a researcher account to reach the htsget-rs endpoint in the data-control account.
- **Researcher_Account**: An external AWS account belonging to a researcher that consumes shared data via VPC Lattice.
- **Test_Lambda**: An optional test Lambda function deployed inside a Researcher_Account's VPC by the htsget VPC Lattice consumer construct (the `deployTestLambda` option). It calls the Htsget_Rs service across VPC Lattice from within the researcher's VPC and returns the response, enabling genuine end-to-end validation of the share path from the consumer side. It is invoked by locating the function whose name contains `TestLambda` and returns `statusCode` 200 with the htsget response under `latticeResponse` on success.
- **Library_ID**: A unique identifier in the OrcaBus system representing a sequencing library and its associated files.
- **Portal_Run_ID**: A unique identifier for a workflow run in the OrcaBus system. In OrcaVault it links a workflow run to its output files. Index sidecar files (`.bai`) are included in a grant because they are present as their own rows in `mart.bam` and are co-located under the same S3 prefix as the corresponding read file.
- **Data_Account**: AWS account (503977275616) that holds the actual bioinformatics data files in S3.
- **Data_Control_Account**: AWS account (916218008751) that hosts htsget-rs and the Authorization_Service, is the owner of all Share_Records regardless of which account the operator authenticates from, and is the Lake Formation consumer account from which the Data_Releaser queries the OrcaVault Mart.
- **Production_Account**: AWS account (472057503814) that hosts the OrcaBus services. OrcaBus is upstream of OrcaVault; the Data_Releaser no longer accesses this account for library resolution.
- **Manifest**: A JSON document listing the library IDs and file references that a researcher has been granted access to.
- **Manifest_Bucket**: A single S3 bucket in the Data_Control_Account that holds every researcher's Manifest, each under a per-account key prefix (`manifests/<researcher-account-id>/manifest.json`). It is owned and provisioned by this project's Infrastructure_Stack, not by any Researcher_Account. Each Researcher_Account is granted read-only access to its own prefix through the bucket's cross-account bucket policy (and, where the bucket is SSE-KMS encrypted, a matching key-policy grant). The Data_Releaser writes manifests to it as a same-account write; the bucket name is supplied to the Data_Releaser through configuration, sourced from an Infrastructure_Stack output.
- **Infrastructure_Stack**: The AWS CDK application in this project's `infrastructure/` directory. It deploys only the Data_Control_Account resources that this project owns and operates: the Share_Record DynamoDB table, the Authorization_Service Lambda functions, the API Gateway REST API fronting them (including its response cache), and the IAM roles those functions require. It does not deploy Htsget_Rs, OrcaHouse/OrcaVault, the LakeFormation_Share, the VPC_Lattice_Share, or any resource in a Researcher_Account or the Data_Account.
- **Access_Rule**: A restriction sub-schema mirroring the htsget-rs `ReferenceNameRestriction`, describing one allowed access scope for a file. It may contain any of `referenceName`, `format` (one of `BAM`, `CRAM`, `VCF`, `BCF`), `start`, and `end` (0-based, `[start, end)`). An absent field means that dimension is unrestricted (allow-all), and multiple Access_Rules for the same file combine as a union. The same Access_Rule sub-schema is used both in the Authorization_Service callout response (`htsgetAuth[].rules`) and in the Manifest (`files[].rules`), so the researcher-facing view matches exactly what Htsget_Rs enforces.

## Requirements

### Requirement 1: Initiate a Data Share

**User Story:** As an administrator, I want to share a set of library IDs with a specific researcher account, so that the researcher can access the associated bioinformatics files via htsget-rs.

#### Acceptance Criteria

1. WHEN the administrator invokes the Data_Releaser with a list of one or more library IDs and a target Researcher_Account ID, THE Data_Releaser SHALL determine each library's shareability by querying the OrcaVault Mart (`mart.bam`) via Athena filtered to `format = 'bam'` and confirming that at least one aligned-read file row is returned for each library ID. A library ID that returns no such rows is treated as having no shareable reads per Acceptance Criterion 1.6.
2. IF the administrator provides an empty list of library IDs or omits the Researcher_Account ID, THEN THE Data_Releaser SHALL reject the request with an error message indicating the missing or invalid input and SHALL NOT issue any Athena query against the OrcaVault Mart.
3. WHEN resolving the requested library IDs, THE Data_Releaser SHALL resolve all associated read files with a single Athena query against `mart.bam` restricted to `format = 'bam'`, retrieving the `bucket`, `key`, `portal_run_id`, `storage_class`, and `format` for each file. The `.bai` index rows are captured under the same prefix. Variant (VCF/BCF) files are out of scope for the current version because no variant mart exists yet.
4. WHEN resolving shared locations, THE Data_Releaser SHALL express each shared location as an S3 prefix (the parent prefix of the resolved read file's `key`, that is the directory containing the `.bam` and its co-located `.bai` index) rather than an individual object key, so that htsget index sidecar files (for example `.bai`) co-located under the prefix are included in the grant.
5. WHEN the shared locations are resolved, THE Data_Releaser SHALL create a Share_Record in the Authorization_Service containing the library IDs, associated S3 prefixes, target Researcher_Account ID, creation timestamp (UTC), and an optional expiry timestamp if a duration was explicitly specified (defaulting to unlimited/no expiry per Requirement 2).
6. IF any library ID returns no aligned-read (`format = 'bam'`) rows from `mart.bam` (no shareable reads), THEN THE Data_Releaser SHALL output the list of affected library IDs to standard error with a message noting that each such ID may not exist, may not be aligned yet, or may have reads that are archived, and abort the share operation without creating partial records.
7. IF a resolved read file reports an archived `storage_class` (for example `DeepArchive` or any Glacier tier) such that htsget-rs could not stream it without a restore, THEN THE Data_Releaser SHALL treat that file as not shareable, report the affected library IDs and archived locations to standard error, and abort the share operation without creating partial records.
8. IF the OrcaVault Mart cannot be queried (Athena or the Lake Formation share is unavailable, or a query completes in a non-success state) or the Authorization_Service is unreachable or returns a non-success response, THEN THE Data_Releaser SHALL report an error message indicating the failed component and abort the operation without creating partial records.
9. WHEN the Share_Record is persisted, THE Data_Releaser SHALL output a confirmation to standard output including the share identifier, the number of libraries shared, and the expiry date.

### Requirement 2: Time-Limited Shares

**User Story:** As an administrator, I want data shares to optionally have configurable expiry durations, so that access can be automatically revoked after an allowed period when needed, while defaulting to unlimited access.

#### Acceptance Criteria

1. WHEN initiating a share, THE Data_Releaser SHALL accept an optional expiry duration parameter with a minimum of 1 day. If no expiry duration is specified, the share SHALL have no expiry (unlimited duration).
2. IF the provided expiry duration is less than 1 day, THEN THE Data_Releaser SHALL reject the share request with an error message indicating the duration is below the minimum allowed.
3. WHEN a share is created with an explicit expiry duration, THE Authorization_Service SHALL store the expiry timestamp (computed as creation time plus the specified duration) as part of the Share_Record. WHEN a share is created without an expiry duration, THE Authorization_Service SHALL store the Share_Record with a null expiry timestamp indicating unlimited access.
4. WHEN Htsget_Rs issues an authorization callout, IF the Share_Record has an expiry timestamp that is in the past or equal to the current time, THEN THE Authorization_Service SHALL respond with an empty authorization rule set denying access.
5. WHEN Htsget_Rs issues an authorization callout, IF the Share_Record has no expiry timestamp (unlimited) or an expiry timestamp in the future, THEN THE Authorization_Service SHALL respond with authorization rules permitting access to the shared files.
6. WHEN Htsget_Rs issues an authorization callout, IF no Share_Record exists for the requested resource and recipient, THEN THE Authorization_Service SHALL respond with an empty authorization rule set denying access.

### Requirement 3: Revoke or Disable a Share

**User Story:** As an administrator, I want to manually revoke an active data share before its expiry, so that I can immediately cut off researcher access when needed.

#### Acceptance Criteria

1. WHEN the administrator invokes the Data_Releaser with a revoke command and a share identifier, THE Data_Releaser SHALL mark the corresponding Share_Record as revoked in the Authorization_Service.
2. WHEN a Share_Record is marked as revoked, THE Authorization_Service SHALL immediately stop returning that share in freshly computed authorization responses. Because authorization responses are cached (see Requirement 13), the revocation SHALL take full effect once the relevant Authorization_Cache entries expire, bounded by the configured Cache_TTL ceiling (which may be up to several hours).
3. IF the share identifier does not exist, THEN THE Data_Releaser SHALL report an error indicating the share was not found.
4. WHEN the revocation is successful, THE Data_Releaser SHALL output a confirmation message including the share identifier and the revocation timestamp.
5. IF the share identifier references a Share_Record that is already revoked, THEN THE Data_Releaser SHALL report an error indicating the share is already revoked.
6. IF the share identifier references a Share_Record that has already expired, THEN THE Data_Releaser SHALL report an error indicating the share has already expired.
7. IF the Authorization_Service is unreachable or returns an error during the revocation operation, THEN THE Data_Releaser SHALL report an error indicating the revocation failed and SHALL NOT alter the Share_Record state.
8. WHERE an administrator requires a revocation to take effect sooner than the Cache_TTL, THE Data_Releaser SHALL attempt a best-effort invalidation of the Authorization_Cache for the affected Researcher_Account, and SHALL communicate that propagation is otherwise bounded by the Cache_TTL.
9. THE library set of a Share_Record SHALL be immutable. THE Data_Releaser SHALL NOT provide in-place editing of a share's library set; to change which libraries a Researcher_Account can access, the administrator SHALL revoke the affected share and create a new share with the desired set (revoke and recreate).

### Requirement 4: Htsget-rs Authorization Callout Integration

**User Story:** As a system architect, I want htsget-rs to query the Authorization_Service at request time (subject to caching), so that access decisions are dynamic and reflect the current state of shares while avoiding a callout on every request.

#### Acceptance Criteria

1. WHEN Htsget_Rs receives an htsget request from a Researcher_Account AND no valid cached authorization decision exists for that requester identity (see Requirement 13), THE Htsget_Rs SHALL issue an authorization callout to the Authorization_Service with the source VPC ARN extracted from the VPC Lattice request headers, within 5 seconds of receiving the request.
2. WHEN the Authorization_Service receives a callout, THE Authorization_Service SHALL look up all active (non-expired, non-revoked) Share_Records whose target_vpc_arn matches the source VPC ARN provided in the callout, and return matching S3 object prefixes as location entries within 3 seconds.
3. THE Authorization_Service SHALL respond with a JSON structure conforming to the htsget-rs authorization response schema (version 1), containing an htsgetAuth array where each entry includes a location (S3 path prefix derived from the Share_Record) and an optional rules array of Access_Rules. WHERE a Share_Record grants unrestricted access to a location, THE Authorization_Service SHALL omit the rules array (full read-only access to that location); WHERE a Share_Record restricts access, THE rules SHALL be populated with Access_Rules describing the permitted scope.
4. WHEN no active shares exist for the requesting VPC ARN, THE Authorization_Service SHALL respond with an empty htsgetAuth array, causing Htsget_Rs to deny the request with an unauthorized response.
5. IF, on a cache miss, the Authorization_Service is unreachable or fails to respond within 5 seconds, THEN THE Htsget_Rs SHALL deny the request (fail-closed) and return an error response indicating the authorization service is unavailable.
6. IF the Authorization_Service returns a response that does not conform to the expected JSON schema, THEN THE Htsget_Rs SHALL deny the request and return an error response indicating an authorization failure.
7. WHILE a cached authorization decision for the requester identity remains within its Cache_TTL, THE Htsget_Rs SHALL authorize the request from the Authorization_Cache without issuing a new callout, as specified in Requirement 13.

### Requirement 5: VPC Lattice Connectivity

**User Story:** As a system architect, I want VPC Lattice to be configured so that researcher accounts can reach the htsget-rs endpoint, so that data can be consumed securely without public internet exposure.

#### Acceptance Criteria

1. THE Data_Control_Account SHALL host a VPC Lattice service exposing the Htsget_Rs Lambda endpoint.
2. THE Data_Control_Account SHALL share the VPC Lattice service network to Researcher_Accounts via AWS RAM.
3. WHEN a new Researcher_Account is onboarded, THE Data_Releaser SHALL verify that an active AWS RAM resource share association exists for the VPC Lattice service network to that account, and IF the association is not active, THEN THE Data_Releaser SHALL display a message indicating the required RAM share is missing and identifying the target account.
4. WHILE no active VPC Lattice service network association exists for a Researcher_Account, THE Data_Releaser SHALL refuse to create Share_Records for that account and display a message indicating which account lacks VPC Lattice connectivity.
5. IF the Data_Releaser cannot query AWS RAM or VPC Lattice APIs due to insufficient permissions or service unavailability, THEN THE Data_Releaser SHALL refuse to create Share_Records and display an error message indicating the connectivity verification failed and the reason.

### Requirement 6: Data Account Cross-Account Access

**User Story:** As a system architect, I want the data-control account to access bioinformatics files in the data account, so that htsget-rs can serve file content to researchers.

#### Acceptance Criteria

1. THE Htsget_Rs deployment SHALL be configured with S3 locations referencing the Data_Account bucket ARN and the IAM role ARN in the Data_Control_Account that has cross-account access permissions.
2. THE Data_Account SHALL have an S3 bucket policy granting s3:GetObject and s3:HeadObject access to the Data_Control_Account IAM role used by Htsget_Rs.
3. IF the Htsget_Rs cannot access a file in the Data_Account due to a permissions or connectivity failure, THEN THE Htsget_Rs SHALL return an error response to the researcher indicating that the requested file is unavailable, without exposing internal account or policy details.
4. WHEN Htsget_Rs receives a valid request for a file stored in the Data_Account, THE Htsget_Rs SHALL retrieve the file content from the Data_Account S3 bucket using the cross-account IAM role and serve it to the researcher.

### Requirement 7: Researcher Manifest Delivery

**User Story:** As a researcher, I want to know which library IDs and files I have been granted access to, so that I can query the correct identifiers via htsget-rs.

#### Acceptance Criteria

1. WHEN a share is created, THE Data_Releaser SHALL generate a Manifest JSON file for the target Researcher_Account describing all libraries the account can currently access, grouped by library, where each library includes its effective expiry and the set of accessible files (each expressed as an htsget request path with optional Access_Rules).
2. WHEN the Manifest is generated or updated, THE Data_Releaser SHALL upload the Manifest to the Manifest_Bucket in the Data_Control_Account at a well-known per-account key path (`manifests/<researcher-account-id>/manifest.json`, one manifest per Researcher_Account). THE Data_Releaser SHALL resolve the Manifest_Bucket name and the key path from configuration (the bucket name sourced from an Infrastructure_Stack output) and SHALL NOT hard-code the bucket name.
3. WHEN a share is revoked or expires and at least one active grant remains for the Researcher_Account, THE Data_Releaser SHALL regenerate the Manifest reflecting only the remaining active (non-revoked, non-expired) access and upload it to the same S3 location.
4. IF all shares for a Researcher_Account are revoked or expired, THEN THE Data_Releaser SHALL delete the Manifest object from the S3 bucket.
5. THE Manifest SHALL include the htsget-rs base URL so that the researcher can construct valid htsget requests.
6. THE Manifest SHALL be a valid JSON document with a top-level structure containing the htsget-rs base URL, the manifest generation timestamp (ISO 8601), and a `libraries` array. Each library entry SHALL include the library ID, an effective expiry date (ISO 8601, or null if unlimited), and a `files` array. Each file entry SHALL include the htsget request path (relative to the base URL, encoding the reads endpoint; variants are out of current scope) and an optional `rules` array of Access_Rules; an absent or empty `rules` array SHALL mean unrestricted access to that file.
7. WHERE a library is granted by more than one active Share_Record for the same Researcher_Account, THE Manifest SHALL express a single effective expiry for that library equal to the latest expiry among those shares, or null if any granting share is unlimited.
8. THE Manifest SHALL be a tool-generated, descriptive artifact that reflects the access a Researcher_Account has been granted but does not itself grant access. Access SHALL be enforced server-side by the Authorization_Service and Htsget_Rs from the Share_Records alone, and the Manifest SHALL NOT be consulted for any authorization decision. Consequently, modifying the Manifest object SHALL have no effect on access, and the Data_Releaser MAY overwrite it on any regeneration.
9. THE Manifest_Bucket SHALL be owned and provisioned by this project's Infrastructure_Stack in the Data_Control_Account (Requirement 16). Each Researcher_Account SHALL be granted read-only access to its own manifest prefix (`manifests/<researcher-account-id>/*`) through the bucket's cross-account bucket policy, managed as part of Data_Control infrastructure and researcher onboarding; the Manifest write itself SHALL be a same-account write performed by the Data_Control_Account identity. THE Data_Releaser SHALL NOT create or delete the Manifest_Bucket.
10. WHEN creating a share, THE Data_Releaser SHALL verify that the configured Manifest_Bucket exists and is writable by the Data_Control_Account identity as part of its preflight checks, before any Share_Record is created. IF the Manifest_Bucket is missing or not writable, THEN THE Data_Releaser SHALL abort with a message naming the expected bucket and indicating that the Infrastructure_Stack is not deployed or is misconfigured, and SHALL NOT create a Share_Record.

### Requirement 8: List Active Shares

**User Story:** As an administrator, I want to view all currently active shares, so that I can audit what data is being shared and to whom.

#### Acceptance Criteria

1. WHEN the administrator invokes the Data_Releaser with a list command, THE Data_Releaser SHALL query the Authorization_Service for all Share_Records and display results ordered by creation date descending.
2. THE Data_Releaser SHALL display each Share_Record including share identifier, target Researcher_Account, library IDs, creation date, expiry date, and status (active, expired, revoked) in a tabular text format with one row per Share_Record.
3. WHERE the administrator specifies a filter by Researcher_Account, THE Data_Releaser SHALL show only Share_Records matching that account.
4. IF the query returns zero Share_Records (or zero matching the applied filter), THEN THE Data_Releaser SHALL display a message indicating no shares were found and exit with a zero exit code.
5. IF the Authorization_Service is unreachable or returns an error, THEN THE Data_Releaser SHALL display an error message indicating the failure reason and exit with a non-zero exit code.
6. IF the administrator specifies a Researcher_Account filter that matches no existing account, THEN THE Data_Releaser SHALL return an empty result set with a message indicating no shares were found for that account.

### Requirement 9: Extend a Share

**User Story:** As an administrator, I want to extend the expiry of an active share, so that a researcher can continue accessing data beyond the original time limit without creating a new share.

#### Acceptance Criteria

1. WHEN the administrator invokes the Data_Releaser with an extend command, a share identifier, and a new duration, THE Data_Releaser SHALL compute the new expiry as the current time plus the specified duration and update the expiry timestamp of the corresponding Share_Record.
2. IF the share has already been revoked or has already expired, THEN THE Data_Releaser SHALL report an error indicating the reason the share cannot be extended and leave the Share_Record unchanged.
3. WHEN the extension is successful, THE Data_Releaser SHALL output a confirmation including the new expiry date and update the Manifest for the affected Researcher_Account.
4. IF the share identifier does not match any existing Share_Record, THEN THE Data_Releaser SHALL report an error indicating that the share was not found.
5. IF the share has no expiry (unlimited), THEN THE Data_Releaser SHALL report that the share already has unlimited duration and leave it unchanged, unless the administrator explicitly intends to add a time limit (in which case the expiry is set as specified).

### Requirement 10: Authorization Service Deployment

**User Story:** As a system architect, I want the authorization service to be a lightweight, low-maintenance component in the data-control account, so that it can respond to htsget-rs callouts with minimal operational overhead.

#### Acceptance Criteria

1. THE Authorization_Service SHALL be deployed as an AWS Lambda function behind an API Gateway in the Data_Control_Account, with API Gateway response caching enabled as specified in Requirement 13.
2. THE Authorization_Service SHALL store Share_Records in a DynamoDB table with the share identifier as the partition key.
3. WHEN the Authorization_Service receives a GET request from Htsget_Rs containing a resource identifier and a requester account ID, THE Authorization_Service SHALL return a response indicating whether access is permitted and, if permitted, which S3 object paths the requester may access.
4. THE Authorization_Service SHALL respond to GET requests from Htsget_Rs within 100ms at the p95 latency under a load of up to 50 concurrent requests.
5. THE Authorization_Service SHALL require no authentication from Htsget_Rs (internal network communication within the same account).
6. IF no matching Share_Record exists for the requested resource and requester account, THEN THE Authorization_Service SHALL return a deny response within the same latency bound.
7. IF the DynamoDB table is unreachable, THEN THE Authorization_Service SHALL return a deny response and include an error indication that distinguishes it from a normal deny.

### Requirement 11: CLI Authentication

**User Story:** As an administrator, I want to run the tool with a single authenticated session into the Data_Control_Account, so that I can create and manage shares without juggling per-account credentials, while the shares remain owned by the Data_Control_Account.

#### Acceptance Criteria

1. THE Data_Releaser SHALL operate under a single set of Data_Control_Account credentials for all commands, resolved via the standard AWS credential chain (for example a named profile backed by AWS SSO / IAM Identity Center, a `credential_process` provider, environment variables, or an assumed role). THE Data_Releaser SHALL NOT require the operator to authenticate into more than one account.
2. THE Data_Control_Account (916218008751) credentials SHALL carry the permissions for every action the tool performs: (a) read-only Athena query of the OrcaVault Mart shared to the Data_Control_Account via Lake Formation (Athena query execution, Glue/Lake Formation catalog read on the `mart` resource-link database, and read of the Athena workgroup S3 results location); (b) read and write of Share_Records in the Authorization_Service and management of VPC Lattice and AWS RAM associations; and (c) same-account write and delete of the Manifest object in the Data_Control_Account Manifest_Bucket.
3. THE Data_Releaser SHALL assume that appropriately-permissioned credentials already exist and SHALL NOT create, define, or modify IAM roles or policies. It SHALL accept the credential source (for example a named profile) via configuration or the default AWS credential chain.
4. THE Data_Control_Account SHALL be the owner of all Share_Records.
5. THE Manifest write SHALL be a same-account write to the Data_Control_Account Manifest_Bucket (Requirement 7.9). Researcher access to the Manifest SHALL be read-only and cross-account, granted by the Manifest_Bucket's bucket policy (and, where the bucket uses SSE-KMS, a corresponding key-policy grant) to each Researcher_Account, scoped to that account's manifest prefix. THE bucket and its cross-account read grants SHALL be provisioned by the Infrastructure_Stack and researcher onboarding (Requirements 16, 17), not by the Data_Releaser, which SHALL NOT create or modify them.
6. IF the Data_Control_Account credentials cannot be resolved, or lack a permission required by the invoked command, THEN THE Data_Releaser SHALL output an error message to stderr describing the failure and exit with a non-zero exit code without performing partial actions.

### Requirement 12: Idempotent Share Creation

**User Story:** As an administrator, I want share creation to be idempotent, so that re-running the same command does not create duplicate shares.

#### Acceptance Criteria

1. WHEN the administrator creates a share with the same set of library IDs (matched regardless of order) and the same target Researcher_Account as an existing active share, THE Data_Releaser SHALL return the existing share identifier rather than creating a duplicate.
2. WHEN a matching active share is found during share creation, THE Data_Releaser SHALL display the existing share's identifier, target Researcher_Account, library IDs, and expiry date, along with an indication that an existing share was reused.
3. IF the administrator creates a share where the library IDs and target Researcher_Account match an existing active share but the requested expiry duration differs from the existing share's expiry, THEN THE Data_Releaser SHALL return the existing share identifier without modifying its expiry and indicate that the existing share's expiry was retained.
4. IF the administrator creates a share where the library IDs partially overlap with an existing active share for the same Researcher_Account but are not an exact set match, THEN THE Data_Releaser SHALL create a new share with the requested library IDs.

### Requirement 13: Authorization Response Caching

**User Story:** As a system architect, I want htsget-rs to reuse authorization decisions rather than call the Authorization_Service on every request, so that latency and load are reduced. This is safe because shares are long-lived (minimum 1 day, often unlimited), and a bounded staleness of up to several hours is acceptable.

#### Acceptance Criteria

1. WHEN the Authorization_Service returns an authorization response permitting access, THE Authorization_Service SHALL include a `Cache-Control: max-age=<seconds>` header where `<seconds>` is the minimum remaining lifetime among the matched active Share_Records, capped at a configurable Cache_TTL ceiling (default 1 hour, maximum 24 hours). WHERE a matched share has no expiry (unlimited), THE Authorization_Service SHALL use the ceiling value for that share.
2. WHEN the Authorization_Service returns a deny response (empty htsgetAuth array), THE Authorization_Service SHALL set a short `max-age` (default 60 seconds, configurable) so that newly created shares become visible promptly (negative caching).
3. THE Data_Control_Account SHALL enable API Gateway response caching on the Authorization_Service endpoint, with the cache key incorporating the forwarded requester identity (source VPC ARN) so that a cached response for one Researcher_Account is never served to another.
4. THE API Gateway cache entry time-to-live SHALL be aligned with the `max-age` emitted by the Authorization_Service, not exceeding the configured Cache_TTL ceiling.
5. THE Htsget_Rs authorization callout client SHALL cache authorization responses within its Lambda execution environment, keyed on the forwarded requester identity, and SHALL reuse a cached response for the duration of the response's `max-age` without issuing a new callout. This in-process cache is best-effort and its lifetime is bounded by the duration of the warm execution environment.
6. WHILE a cached authorization decision exists and has not exceeded its `max-age`, THE Htsget_Rs SHALL authorize the request from cache without contacting the Authorization_Service (neither the API Gateway cache nor the Lambda is invoked when the in-process cache is warm).
7. WHEN a cached authorization decision has exceeded its `max-age`, THE Htsget_Rs SHALL issue a fresh authorization callout before authorizing the request.
8. THE Authorization_Service SHALL cache DynamoDB lookups within its own Lambda execution environment, keyed on requester identity and bounded by the `max-age`, to avoid repeated DynamoDB reads on warm invocations. This cache is best-effort.
9. THE cache key for every caching layer (API Gateway, Htsget_Rs in-process, and Authorization_Service in-process) SHALL incorporate the requester identity, such that authorization decisions are never shared across Researcher_Accounts.
10. IF a caching layer is unavailable or a cached entry is absent, THEN the system SHALL fall back to a fresh authorization callout as described in Requirement 4, and correctness SHALL NOT depend on any cache being present.

### Requirement 14: Htsget-rs Callout Caching Implementation

**User Story:** As a maintainer of the UMCCR htsget-rs repo, I want the authorization callout client to support response caching, so that the caching behavior in Requirement 13 can be realized. The current callout implementation (the `[auth.authorization]` `kind = "callout"` source, defined in `htsget-config`) issues a request per htsget request, exposes only `url`, `http`, and `forward` options with no cache/TTL controls, and the response schema (`AuthorizationRestrictions`) carries no cache directive, so this capability must be added.

#### Acceptance Criteria

1. THE Htsget_Rs authorization callout client SHALL be extended to cache authorization responses in-process (within the Lambda execution environment), keyed on the forwarded requester identity (source VPC ARN).
2. THE Htsget_Rs authorization callout client SHALL honor the `Cache-Control: max-age` directive returned by the Authorization_Service, reusing a cached response until the max-age elapses before issuing a new callout.
3. THE htsget-rs callout configuration SHALL expose options to enable or disable the cache and to set a maximum TTL ceiling that caps any `max-age` returned by the Authorization_Service.
4. WHERE no `Cache-Control` directive is present in a response, THE Htsget_Rs client SHALL treat the response as non-cacheable or apply a configured default TTL, and SHALL NOT cache the response indefinitely.
5. THE cache implementation SHALL be safe for concurrent access within a single execution environment and SHALL scope entries to the requester identity so that a cached decision is never reused across different requesters.
6. WHEN a cached entry is absent or has expired, THE Htsget_Rs client SHALL issue a fresh callout and populate the cache from the new response.
7. THE implementation SHALL be contributed to the UMCCR htsget-rs repo, touching the callout/auth configuration and HTTP client layers in `htsget-config` and the crate that performs the callout, and SHALL be covered by unit and integration tests.

### Requirement 15: Infrastructure Validation (Preflight / Test Setup)

**User Story:** As an administrator, I want a command that validates the deployed sharing infrastructure end to end, so that I can confirm the system is healthy after deployment and before onboarding a researcher, independently of creating a real production share.

#### Acceptance Criteria

1. THE Data_Releaser SHALL provide a `validate` command that checks the health and reachability of the sharing infrastructure and reports a per-check pass/fail diagnostic without creating or modifying any persistent Share_Record.
2. WHEN the `validate` command is invoked without a target Researcher_Account, THE Data_Releaser SHALL perform provider-side checks, including: (a) that the Data_Control_Account credentials resolve and carry the required permissions per Requirement 11; (b) that the OrcaVault Mart is reachable via Athena through the Lake Formation share and returns schema-conformant results for a probe query (for example a `LIMIT 1` against `mart.bam`); (c) that the Authorization_Service is reachable and returns a schema-conformant response per Requirements 4 and 13; (d) that the Htsget_Rs endpoint is deployed and responds to a service-info or health request; and (e) that Htsget_Rs cross-account access to the Data_Account S3 bucket is functional per Requirement 6 (for example, a HeadObject on a known canary object).
3. WHEN the `validate` command is invoked with a target Researcher_Account, THE Data_Releaser SHALL additionally verify that the VPC Lattice service network association to that account is present and in an active/associated state and that the AWS RAM resource share is accepted by the target account, per Requirement 5.
4. WHERE the Test_Lambda is present in the target Researcher_Account, THE Data_Releaser SHALL perform a genuine end-to-end check by discovering and invoking the Test_Lambda using the Data_Control_Account credentials (the Test_Lambda's resource policy SHALL permit the Data_Control_Account to list and invoke it), and SHALL treat the check as passing only if the invocation returns a success status (statusCode 200) with a valid htsget service response under `latticeResponse`.
5. IF the Test_Lambda is not deployed in the target Researcher_Account, THEN THE Data_Releaser SHALL report that the in-VPC end-to-end probe is unavailable and SHALL recommend deploying it via the consumer construct's `deployTestLambda` option, WHILE still reporting the results of the provider-side and association checks it was able to perform.
6. WHERE a canary dataset is configured, THE Data_Releaser SHALL support an optional test-setup mode that creates a short-lived synthetic Share_Record for the canary data, invokes the Test_Lambda to confirm the canary data is retrievable end to end, and then removes the synthetic Share_Record, reporting whether the simulated share path succeeded.
7. IF any validation check fails, THEN THE Data_Releaser SHALL report which check failed and the reason and SHALL exit with a non-zero exit code, WHILE continuing to run and report the remaining independent checks rather than aborting on the first failure.
8. THE `validate` command SHALL be side-effect-free except for the optional synthetic canary share in test-setup mode, which SHALL be cleaned up before the command exits, including on error (best-effort).
9. WHEN the `validate` command is invoked with a target Researcher_Account, THE Data_Releaser SHALL additionally verify that the Manifest_Bucket's cross-account bucket policy grants that Researcher_Account read access to its own manifest prefix, and IF the grant is absent, THEN THE Data_Releaser SHALL report that the researcher's cross-account read grant is missing and that the onboarding step which adds it to the Manifest_Bucket policy is incomplete.
10. WHEN the `validate` command is invoked, THE Data_Releaser SHALL additionally verify that the Data_Control_Account resources deployed by the Infrastructure_Stack are present and consistent with configuration, including the Share_Record DynamoDB table, the Manifest_Bucket (present and writable by the Data_Control_Account identity), and the API Gateway REST API stage named in configuration, and SHALL report which deployment step is missing when a resource cannot be found.

### Requirement 16: Infrastructure Deployment

**User Story:** As an operator, I want the AWS resources this project owns to be deployed from this repository as code, so that the Authorization_Service and its data store are reproducible, reviewable, and versioned alongside the CLI that depends on them.

#### Acceptance Criteria

1. THE project SHALL include an AWS CDK application in an `infrastructure/` directory that deploys, into the Data_Control_Account, every AWS resource required for the Authorization_Service to operate: the Share_Record DynamoDB table, the Authorization_Service Lambda function(s), the API Gateway REST API and stage fronting them, the IAM execution roles those functions require, and the Manifest_Bucket together with its cross-account read bucket policy.
2. THE Infrastructure_Stack SHALL provision the Share_Record DynamoDB table with `shareId` as the partition key and the two global secondary indexes the access patterns require: one keyed on `targetVpcArn` for authorization callout lookup (Requirement 4.2) and one keyed on `researcherAccountId` for listing and idempotency (Requirements 8 and 12). It SHALL NOT configure a DynamoDB TTL that deletes expired Share_Records, because expired and revoked records must remain visible to `list`.
3. THE Infrastructure_Stack SHALL front the Authorization_Service with an API Gateway REST API (not an HTTP API), because Requirement 13.3 depends on API Gateway response caching and Requirement 3.8 depends on the `FlushStageCache` operation, neither of which HTTP APIs provide. It SHALL enable response caching on the deployed stage and configure the cache key to incorporate the forwarded requester identity per Requirement 13.3.
4. THE Infrastructure_Stack SHALL expose the deployed REST API identifier, stage name, and Manifest_Bucket name as stack outputs, so that the values the Data_Releaser configuration requires (`endpoints.apiGatewayRestApiId`, `endpoints.apiGatewayStageName`, `manifest.bucket`) can be read from the deployment rather than guessed.
5. THE Infrastructure_Stack SHALL provision the Manifest_Bucket as a single private S3 bucket in the Data_Control_Account holding one manifest per Researcher_Account under a per-account key prefix (`manifests/<researcher-account-id>/manifest.json`). Its bucket policy SHALL grant each onboarded Researcher_Account read-only access (`s3:GetObject`) scoped to that account's prefix and nothing else; the set of granted accounts SHALL be a parameter of the stack so onboarding a researcher is a configuration change rather than a code change. The bucket SHALL block public access and, where encrypted with SSE-KMS, its key policy SHALL grant the same per-account decrypt access.
6. THE project SHALL provide deployable Lambda entrypoints for the Authorization_Service: one for the authorization callout interface used by Htsget_Rs and one for the admin interface used by the Data_Releaser CLI. These SHALL reuse the existing handler logic rather than duplicating it.
7. THE Infrastructure_Stack SHALL provision the IAM execution roles for the Lambda functions it deploys, scoped to the DynamoDB table and indexes those functions use, and SHALL NOT grant broader access than the handlers require.
8. THE Infrastructure_Stack SHALL NOT deploy Htsget_Rs, OrcaHouse/OrcaVault, the LakeFormation_Share, the VPC_Lattice_Share, the Data_Account S3 bucket or its policy, or any resource in a Researcher_Account. Those are deployed and owned elsewhere, per Requirement 17.
9. THE Infrastructure_Stack MAY provision the administrator IAM policy described in Requirement 11.2 for operators of the Data_Releaser. WHERE it does, this SHALL NOT change Requirement 11.3: the Data_Releaser CLI itself SHALL still create no IAM resources at runtime.
10. THE Infrastructure_Stack SHALL be deployable to more than one environment (for example a development and a production Data_Control_Account) without source changes, by parameterising account, region, and resource naming.

### Requirement 17: External Deployment Assumptions

**User Story:** As an operator onboarding a new researcher or standing up a new environment, I want the assumptions this project makes about resources deployed elsewhere to be stated explicitly, so that I can tell which failures are mine to fix and which belong to another project.

#### Acceptance Criteria

1. THE project SHALL document, per AWS account, the resources it assumes exist but does not deploy, and SHALL identify which are prerequisites for the Infrastructure_Stack, which are prerequisites for running the Data_Releaser CLI, and which are prerequisites for a Researcher_Account to consume data.
2. THE project SHALL assume the following in the Warehouse_Account (115253169271), deployed and owned by OrcaHouse: OrcaVault and its curated `mart` layer, including `mart.bam`, and the LakeFormation_Share exposing that Mart to the Data_Control_Account.
3. THE project SHALL assume the following in the Data_Control_Account (916218008751), deployed and owned elsewhere: the Htsget_Rs Lambda deployment and its `[auth.authorization]` callout configuration pointing at the Authorization_Service; the VPC Lattice service exposing the Htsget_Rs endpoint and its VPC_Lattice_Share via AWS RAM; the Lake Formation resource-link database (`mart`) consuming the LakeFormation_Share together with `SELECT`/`DESCRIBE` grants to local principals; the Athena workgroup and its S3 results location; and the IAM role Htsget_Rs uses for cross-account reads from the Data_Account.
4. THE project SHALL assume the following in the Data_Account (503977275616), deployed and owned elsewhere: the S3 bucket holding the bioinformatics files, and the bucket policy granting `s3:GetObject` and `s3:HeadObject` to the Data_Control_Account role used by Htsget_Rs per Requirement 6.2.
5. THE project SHALL assume the following in each Researcher_Account, deployed and owned by that account's onboarding deployment: the accepted AWS RAM resource share and active VPC Lattice service network association per Requirement 5; an IAM identity-based policy permitting the account's own principals to read the Manifest from the Data_Control_Account Manifest_Bucket (the resource-side grant is on the Manifest_Bucket policy and is owned by Data_Control per Requirements 7.9, 11.5, and 16.5 — a cross-account read requires both sides); and optionally the Test_Lambda used by `validate` per Requirement 15.4.
6. THE project SHALL assume that Data_Control_Account credentials with the permissions enumerated in Requirement 11.2 are provisioned and available to the operator through the standard AWS credential chain.
7. WHERE an assumed external resource is missing or misconfigured, THE Data_Releaser SHALL report which prerequisite is unmet and which account owns it, rather than reporting a bare AWS API error. THE `validate` command SHALL be the primary means of checking these assumptions, per Requirement 15.

### Requirement 18: Documentation

**User Story:** As an administrator or operator new to this project, I want documentation that tells me how to deploy it, how to run it, and what it assumes about the surrounding accounts, so that I can operate the tool without reading its source.

#### Acceptance Criteria

1. THE project SHALL include a top-level README describing what the Data_Releaser does, the account topology it operates across, and how the CLI, the Authorization_Service, Htsget_Rs, and the OrcaVault Mart relate to one another.
2. THE project SHALL document how to build and run the CLI, including every subcommand (`share`, `revoke`, `extend`, `list`, `validate`), its flags, worked invocation examples with representative output, and the exit-code convention.
3. THE project SHALL document the configuration file: every field, which fields are required, default values, how CLI flags override file values, the locations searched when `--config` is not supplied, and a complete annotated example.
4. THE project SHALL document how to deploy the Infrastructure_Stack, including prerequisites, the CDK commands to synthesise and deploy, how to target a specific account and region, the stack outputs produced, and how those outputs map onto the CLI configuration fields that reference them.
5. THE project SHALL document the external deployment assumptions from Requirement 17 as a per-account prerequisite checklist, stating for each item which project owns it and how to verify it is in place.
6. THE project SHALL document the Researcher_Account onboarding procedure end to end: the RAM share acceptance and VPC Lattice association, the Manifest_Bucket and its bucket policy, the optional Test_Lambda, and how to confirm the result with `validate` before the first real share.
7. THE project SHALL document the operational characteristics an administrator must understand to use the tool safely: that revocation propagation is bounded by the Cache_TTL rather than immediate (Requirement 3.2), that `--flush-cache` shortens but does not eliminate that window, that library sets are immutable and changes require revoke-and-recreate (Requirement 3.9), that share creation is idempotent (Requirement 12), and that warehouse resolution reflects the most recent daily OrcaVault build and may lag live OrcaBus state by up to roughly a day.
8. THE project SHALL document the Manifest: its JSON schema, where it is written, and explicitly that it is descriptive only and never consulted for authorization (Requirement 7.8), so that researchers do not treat it as a control surface.
9. THE project SHALL document the htsget-rs callout caching contribution from Requirement 14, including which upstream repository and crates it targets and the configuration it introduces, so that the dependency between this project and the upstream change is traceable.
10. THE documentation SHALL be kept consistent with the implemented behaviour; WHERE a command, flag, configuration field, or deployment step changes, the corresponding documentation SHALL be updated in the same change.
ated in the same change.
