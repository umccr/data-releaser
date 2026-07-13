# Requirements Document

## Introduction

The Data Releaser is a CLI tool that enables UMCCR administrators to share bioinformatics data (identified by `library_id`) with external researchers via the htsget-rs protocol. The tool coordinates data sharing across multiple AWS accounts by managing an authorization backend that htsget-rs queries to determine access permissions, and by maintaining VPC Lattice connectivity to researcher accounts. Researchers consume shared data through Seqera via a VPC Lattice connection to an htsget-rs endpoint in the data-control account.

## Glossary

- **Data_Releaser**: The Go CLI tool that administrators use to initiate, manage, and revoke data shares.
- **Credential_Broker**: The component within the Data_Releaser that obtains and vends purpose-scoped AWS credentials to each downstream client (OrcaBus API access, Authorization_Service access, Researcher_Account manifest writes). It is credential-source-agnostic: each purpose is backed by an independently configured credential source, which may be a named profile (for example one backed by AWS SSO / IAM Identity Center or a `credential_process` provider), an assumed role ARN, or static credentials. Sources are resolved independently via the standard AWS credential chain and need not share a base identity.
- **Authorization_Service**: A lightweight backend service deployed in the data-control account that stores share records and responds to htsget-rs authorization callouts with access rules.
- **Authorization_Cache**: The combined caching layer for authorization decisions, comprising (a) an API Gateway response cache in front of the Authorization_Service and (b) an in-execution-environment cache within the Htsget_Rs Lambda. Both layers are keyed on the requester identity so decisions remain isolated per Researcher_Account.
- **Cache_TTL**: The maximum time a cached authorization response may be reused before a fresh callout is required. It is driven by the `Cache-Control: max-age` emitted by the Authorization_Service and capped at a configurable ceiling. The Cache_TTL bounds how long a revocation or expiry may take to propagate.
- **Share_Record**: A persistent record describing which library IDs are shared to which AWS account, including expiry and status.
- **Workflow_Manager**: The OrcaBus service-workflow-manager that tracks workflow runs and their associated libraries. It provides the mapping from library IDs to portal_run_ids via a many-to-many relationship between WorkflowRun and Library models, queryable at `/api/v1/workflowrun?libraries__library_id=<value>`.
- **Filemanager**: The OrcaBus service-filemanager that indexes S3 objects and provides an API to query files by attributes. Files are queryable by `portalRunId` attribute (e.g. `/api/v1/s3/attributes?portalRunId=<value>`), which is set on records by the FMAnnotator service. The Filemanager does not store or query by library ID directly.
- **FMAnnotator**: The OrcaBus service-fmannotator, an event-driven service (no API) that listens to `WorkflowRunStateChange` events and tags Filemanager S3 object records with the `portalRunId` attribute by matching the portal_run_id in the S3 key path.
- **Metadata_Manager**: The OrcaBus service-metadata-manager that holds canonical library metadata (phenotype, type, quality, assay, coverage, sample, subject, project). It can validate that a library ID exists but does not map library IDs to file paths or portal_run_ids.
- **Htsget_Rs**: The htsget-rs server deployed in the data-control account as an AWS Lambda, serving bioinformatics data via the GA4GH htsget protocol.
- **VPC_Lattice_Share**: An AWS VPC Lattice service network share via AWS RAM that allows a researcher account to reach the htsget-rs endpoint in the data-control account.
- **Researcher_Account**: An external AWS account belonging to a researcher that consumes shared data via VPC Lattice.
- **Test_Lambda**: An optional test Lambda function deployed inside a Researcher_Account's VPC by the htsget VPC Lattice consumer construct (the `deployTestLambda` option). It calls the Htsget_Rs service across VPC Lattice from within the researcher's VPC and returns the response, enabling genuine end-to-end validation of the share path from the consumer side. It is invoked by locating the function whose name contains `TestLambda` and returns `statusCode` 200 with the htsget response under `latticeResponse` on success.
- **Library_ID**: A unique identifier in the OrcaBus system representing a sequencing library and its associated files.
- **Portal_Run_ID**: A unique identifier for a workflow run in the OrcaBus system. The portal_run_id is embedded in S3 object key paths and is used as the key attribute to link workflow runs to their output files in the Filemanager.
- **Data_Account**: AWS account (503977275616) that holds the actual bioinformatics data files in S3.
- **Data_Control_Account**: AWS account (916218008751) that hosts htsget-rs and the Authorization_Service, and is the owner of all Share_Records regardless of which account the operator authenticates from.
- **Production_Account**: AWS account (472057503814) that hosts OrcaBus services.
- **Manifest**: A JSON document listing the library IDs and file references that a researcher has been granted access to.
- **Access_Rule**: A restriction sub-schema mirroring the htsget-rs `ReferenceNameRestriction`, describing one allowed access scope for a file. It may contain any of `referenceName`, `format` (one of `BAM`, `CRAM`, `VCF`, `BCF`), `start`, and `end` (0-based, `[start, end)`). An absent field means that dimension is unrestricted (allow-all), and multiple Access_Rules for the same file combine as a union. The same Access_Rule sub-schema is used both in the Authorization_Service callout response (`htsgetAuth[].rules`) and in the Manifest (`files[].rules`), so the researcher-facing view matches exactly what Htsget_Rs enforces.

## Requirements

### Requirement 1: Initiate a Data Share

**User Story:** As an administrator, I want to share a set of library IDs with a specific researcher account, so that the researcher can access the associated bioinformatics files via htsget-rs.

#### Acceptance Criteria

1. WHEN the administrator invokes the Data_Releaser with a list of one or more library IDs and a target Researcher_Account ID, THE Data_Releaser SHALL validate that each library ID exists by querying the Workflow_Manager API (filtering workflow runs by library ID) and confirm that at least one workflow run is associated with each library ID.
2. IF the administrator provides an empty list of library IDs or omits the Researcher_Account ID, THEN THE Data_Releaser SHALL reject the request with an error message indicating the missing or invalid input and SHALL NOT query the Workflow_Manager or Filemanager APIs.
3. WHEN all library IDs are validated, THE Data_Releaser SHALL resolve all associated S3 object paths by first querying the Workflow_Manager API to obtain the portal_run_ids for each library ID, and then querying the Filemanager API by portalRunId attribute to retrieve the corresponding S3 object records.
4. WHEN the S3 object paths are resolved, THE Data_Releaser SHALL create a Share_Record in the Authorization_Service containing the library IDs, associated S3 paths, target Researcher_Account ID, creation timestamp (UTC), and an optional expiry timestamp if a duration was explicitly specified (defaulting to unlimited/no expiry per Requirement 2).
5. IF any library ID does not have an associated workflow run in the Workflow_Manager, or if a resolved portal_run_id has no matching S3 objects in the Filemanager, THEN THE Data_Releaser SHALL output the list of invalid or unmapped library IDs to standard error and abort the share operation without creating partial records.
6. IF the Workflow_Manager API, the Filemanager API, or the Authorization_Service is unreachable or returns a non-success response, THEN THE Data_Releaser SHALL report an error message indicating the failed service and abort the operation without creating partial records.
7. WHEN the Share_Record is persisted, THE Data_Releaser SHALL output a confirmation to standard output including the share identifier, the number of libraries shared, and the expiry date.

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
2. WHEN the Manifest is generated or updated, THE Data_Releaser SHALL upload the Manifest to an S3 bucket in the target Researcher_Account at a well-known object key path per account (one manifest per Researcher_Account).
3. WHEN a share is revoked or expires and at least one active grant remains for the Researcher_Account, THE Data_Releaser SHALL regenerate the Manifest reflecting only the remaining active (non-revoked, non-expired) access and upload it to the same S3 location.
4. IF all shares for a Researcher_Account are revoked or expired, THEN THE Data_Releaser SHALL delete the Manifest object from the S3 bucket.
5. THE Manifest SHALL include the htsget-rs base URL so that the researcher can construct valid htsget requests.
6. THE Manifest SHALL be a valid JSON document with a top-level structure containing the htsget-rs base URL, the manifest generation timestamp (ISO 8601), and a `libraries` array. Each library entry SHALL include the library ID, an effective expiry date (ISO 8601, or null if unlimited), and a `files` array. Each file entry SHALL include the htsget request path (relative to the base URL, encoding the reads or variants endpoint) and an optional `rules` array of Access_Rules; an absent or empty `rules` array SHALL mean unrestricted access to that file.
7. WHERE a library is granted by more than one active Share_Record for the same Researcher_Account, THE Manifest SHALL express a single effective expiry for that library equal to the latest expiry among those shares, or null if any granting share is unlimited.

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

### Requirement 11: CLI Authentication and Credential Sourcing

**User Story:** As a security-conscious administrator, I want the CLI tool to obtain purpose-scoped credentials for each account and action it needs from whatever credential source I already use, so that data shares are created with least-privilege access and only by authorized personnel, while the shares themselves remain owned by the Data_Control_Account.

#### Acceptance Criteria

1. THE Data_Releaser SHALL obtain credentials for each distinct purpose through a Credential_Broker that resolves an independently configured credential source for that purpose using the standard AWS credential chain. A credential source MAY be a named profile (for example one backed by AWS SSO / IAM Identity Center or a `credential_process` provider), an assumed role ARN, or static credentials. THE Data_Releaser SHALL NOT assume that the purposes share a common base identity, and SHALL NOT require any account to trust another account or a shared identity.
2. THE distinct credential purposes are:
   a. OrcaBus metadata access (in the account hosting OrcaBus) permitting retrieval of the OrcaBus API token (`orcabus/token-service-jwt`) and/or read access to the Workflow_Manager and Filemanager APIs;
   b. Data_Control_Account access permitting read and write of Share_Records in the Authorization_Service and management of VPC Lattice and AWS RAM associations;
   c. Researcher_Account access permitting write and delete of the Manifest object.
3. THE Data_Releaser SHALL assume that appropriately-permissioned credential sources and IAM roles already exist and SHALL NOT create, define, or modify them. THE Data_Releaser SHALL accept the per-purpose credential source identifiers (named profiles, role ARNs, or equivalent) via configuration.
4. WHERE a purpose's credential source is a named profile backed by SSO or a `credential_process` provider, THE Data_Releaser SHALL delegate credential resolution to that provider via the AWS SDK and SHALL NOT require the operator to pre-authenticate through any tool-specific mechanism.
5. THE Data_Control_Account (916218008751) SHALL be the owner of all Share_Records, and THE Data_Releaser SHALL create and manage Share_Records exclusively through the Data_Control_Account credential source, regardless of which account or identity the other purposes resolve to.
6. THE Credential_Broker SHALL vend the correct purpose-scoped credentials to each downstream client (OrcaBus API client, Authorization_Service client, Researcher_Account S3 client) and SHALL NOT reuse one purpose's credentials for another purpose.
7. THE Data_Releaser SHALL resolve each purpose's credentials independently and SHALL NOT chain credential resolution (for example, it SHALL NOT use the Data_Control_Account credentials to obtain the OrcaBus metadata credentials), so as to avoid cross-account coupling and role-chaining session limits.
8. WHERE the Researcher_Account is external and cannot be reached by a credential source the operator holds directly (for example, an SSO profile into that account), THE Data_Releaser SHALL support resolving the Researcher_Account purpose through a cross-account IAM role that the researcher provisions and configures to trust the operator's principal, or through an equivalent bucket-policy grant to the operator's principal.
9. WHERE an operation requires only a subset of purposes (for example, a list or revoke command that only touches Share_Records), THE Data_Releaser SHALL resolve only the credential source or sources needed for that operation.
10. THE effective authorization gate for performing a share SHALL be the operator's ability to resolve the required credential sources; THE Data_Releaser SHALL NOT rely on a hard-coded check that the operator belongs to a specific account.
11. IF a required credential source cannot be resolved (including a failed SSO/`credential_process` invocation or a failed role assumption), THEN THE Data_Releaser SHALL output an error message to stderr identifying which purpose could not be resolved, and exit with a non-zero exit code without performing partial actions.

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
2. WHEN the `validate` command is invoked without a target Researcher_Account, THE Data_Releaser SHALL perform provider-side checks, including: (a) that all required IAM roles can be assumed per Requirement 11; (b) that the Workflow_Manager and Filemanager APIs are reachable and authenticated; (c) that the Authorization_Service is reachable and returns a schema-conformant response per Requirements 4 and 13; (d) that the Htsget_Rs endpoint is deployed and responds to a service-info or health request; and (e) that Htsget_Rs cross-account access to the Data_Account S3 bucket is functional per Requirement 6 (for example, a HeadObject on a known canary object).
3. WHEN the `validate` command is invoked with a target Researcher_Account, THE Data_Releaser SHALL additionally verify that the VPC Lattice service network association to that account is present and in an active/associated state and that the AWS RAM resource share is accepted by the target account, per Requirement 5.
4. WHERE the Test_Lambda is present in the target Researcher_Account, THE Data_Releaser SHALL perform a genuine end-to-end check by discovering and invoking the Test_Lambda using the Researcher_Account role (which SHALL permit listing and invoking the Test_Lambda), and SHALL treat the check as passing only if the invocation returns a success status (statusCode 200) with a valid htsget service response under `latticeResponse`.
5. IF the Test_Lambda is not deployed in the target Researcher_Account, THEN THE Data_Releaser SHALL report that the in-VPC end-to-end probe is unavailable and SHALL recommend deploying it via the consumer construct's `deployTestLambda` option, WHILE still reporting the results of the provider-side and association checks it was able to perform.
6. WHERE a canary dataset is configured, THE Data_Releaser SHALL support an optional test-setup mode that creates a short-lived synthetic Share_Record for the canary data, invokes the Test_Lambda to confirm the canary data is retrievable end to end, and then removes the synthetic Share_Record, reporting whether the simulated share path succeeded.
7. IF any validation check fails, THEN THE Data_Releaser SHALL report which check failed and the reason and SHALL exit with a non-zero exit code, WHILE continuing to run and report the remaining independent checks rather than aborting on the first failure.
8. THE `validate` command SHALL be side-effect-free except for the optional synthetic canary share in test-setup mode, which SHALL be cleaned up before the command exits, including on error (best-effort).
