# Implementation Plan: Data Releaser CLI

## Overview

The Data Releaser is a Go CLI tool that grants external researchers time-bounded (or unlimited) access to bioinformatics libraries via htsget-rs. Implementation proceeds from foundational types and configuration, through core subsystem clients (warehouse resolution, authorization service, VPC Lattice/RAM, manifest writer), to command orchestration, wiring, and validation.

Three further deliverables follow the CLI. The Authorization Service handlers need deployable Lambda entrypoints (`functions/`) and an AWS CDK application (`infrastructure/`) that provisions the Data Control account resources this project owns — the DynamoDB table, the Lambda functions, and the API Gateway in front of them (tasks 21, 22; Requirement 16). Nothing outside that account is deployed here (Requirement 17). The htsget-rs callout caching extension (Requirement 14) is an upstream contribution to `umccr/htsget-rs`, worked in a git-ignored sub-clone (task 15). Operator and administrator documentation is a deliverable in its own right (task 24; Requirement 18).

## Tasks

- [x] 1. Project structure, configuration, and core types
  - [x] 1.1 Initialize Go module, directory layout, and CLI skeleton
    - Create `go.mod`, top-level `main.go`, and package directories: `cmd/`, `internal/config/`, `internal/warehouse/`, `internal/auth/`, `internal/lattice/`, `internal/manifest/`, `internal/validate/`, `internal/model/`
    - Set up Cobra (or similar) CLI framework with stub subcommands: `share`, `revoke`, `extend`, `list`, `validate`
    - _Requirements: 1, 3, 8, 9, 15_

  - [x] 1.2 Define core domain types and data models
    - Implement `ShareRecord` struct (shareId ULID, researcherAccountId, targetVpcArn, libraryIds, s3Prefixes, createdAt, expiresAt, revokedAt, status)
    - Implement effective-status derivation logic (active/expired/revoked based on current time)
    - Implement `AccessRule` sub-schema struct (referenceName, format, start, end)
    - Implement `Manifest` struct (htsgetBaseUrl, generatedAt, libraries with files and rules)
    - Implement `ResolvedLibrary` struct (libraryId, portalRunId, bucket, key, format, storageClass)
    - _Requirements: 1.5, 2.3, 7.6, 10.2_

  - [x] 1.3 Implement configuration loading (YAML + flags)
    - Parse config file (profile, warehouse settings, endpoints, cache TTLs, validation canary)
    - Support CLI flag overrides for profile, account, expiry, etc.
    - Validate required fields; reject invalid config early
    - _Requirements: 11.1, 11.3, 13.1, 13.2_

  - [x]* 1.4 Write unit tests for effective-status derivation and config validation
    - Test active/expired/revoked status computation at boundary times
    - Test config parsing with valid, missing, and invalid inputs
    - _Requirements: 2.3, 2.4, 11.1_

- [x] 2. AWS credential resolution and SDK client factory
  - [x] 2.1 Implement credential resolution via standard AWS SDK chain
    - Use AWS SDK for Go v2 `config.LoadDefaultConfig` with optional profile override from config
    - Wrap in a factory that returns configured SDK clients (Athena, DynamoDB, RAM, S3, Lambda, API Gateway)
    - Report clear error on credential failure and exit non-zero without partial actions
    - _Requirements: 11.1, 11.2, 11.3, 11.6_

  - [x]* 2.2 Write unit tests for credential error handling
    - Mock credential resolution failure and verify non-zero exit with descriptive error
    - _Requirements: 11.6_

- [ ] 3. OrcaVault warehouse resolution client
  - [x] 3.1 Implement Athena query builder for library resolution
    - Build parameterized SQL query against `mart.bam` only (no `mart.lims`, no `mart.output`), per design
    - Filter `mart.bam` to `format = 'bam'` only
    - Ensure library IDs are parameterized/validated (no string concatenation) to prevent SQL injection
    - _Requirements: 1.1, 1.3, 1.4_

  - [x] 3.2 Implement Athena query execution and polling
    - Call `StartQueryExecution` with configured workgroup/database/catalog
    - Poll `GetQueryExecution` until SUCCEEDED or FAILED/CANCELLED
    - On non-success terminal state, report the Athena error and abort
    - Parse `GetQueryResults` into `ResolvedLibrary` structs
    - _Requirements: 1.3, 1.8_

  - [x] 3.3 Implement library resolution and prefix derivation
    - Report a library ID with no `format = 'bam'` rows in `mart.bam` as having no shareable reads (message: the ID may not exist, may not be aligned yet, or its reads may be archived)
    - Derive S3 prefix as the parent prefix of the resolved `.bam` `key` (covers the co-located `.bai` index)
    - Detect archived `storage_class` (DeepArchive, Glacier tiers) and abort if found
    - Collect all errors before aborting (report all no-shareable-reads and archived library IDs at once)
    - _Requirements: 1.1, 1.4, 1.6, 1.7_

  - [ ]* 3.4 Write property test for warehouse resolution (Property 4: No partial shares)
    - **Property 4: No partial shares**
    - For any combination of no-shareable-reads and archived libraries in the input, verify that no Share_Record is created and all problematic IDs are reported
    - NOT DONE: no property-testing library is present in `go.mod`; existing coverage is example-based only
    - **Validates: Requirements 1.6, 1.7, 1.8**

  - [x]* 3.5 Write unit tests for Athena query builder and result parsing
    - Test SQL generation with varying library ID counts
    - Test prefix derivation from the `.bam` key
    - Test archived storage_class detection
    - Test no-shareable-reads detection (library ID with no `format = 'bam'` rows)
    - _Requirements: 1.1, 1.3, 1.4, 1.6, 1.7_

- [x] 4. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 5. Authorization Service client (admin interface)
  - [x] 5.1 Implement Share_Record CRUD operations
    - Create: persist ShareRecord with ULID, computed expiry, s3Prefixes, status=active
    - Revoke: set revokedAt timestamp, verify share is active (not already revoked/expired)
    - Extend: update expiresAt to now + duration, verify share is active
    - List: query by researcherAccountId (GSI2) or scan all, ordered by createdAt desc
    - _Requirements: 1.5, 3.1, 9.1, 8.1_

  - [x] 5.2 Implement idempotency check for share creation
    - Query existing active shares for same researcherAccountId (GSI2)
    - Compare library ID sets (order-independent) for exact match
    - If match found: return existing share ID without creating duplicate
    - If partial overlap: proceed with new share creation
    - _Requirements: 12.1, 12.2, 12.3, 12.4_

  - [x] 5.3 Implement state-conflict error handling
    - Revoke on already-revoked: report specific error
    - Revoke on expired: report specific error
    - Extend on revoked/expired: report specific error
    - Unknown share ID: report not-found error
    - All leave state unchanged on conflict
    - _Requirements: 3.3, 3.5, 3.6, 9.2, 9.4_

  - [ ]* 5.4 Write property test for idempotent creation (Property 10)
    - **Property 10: Idempotent creation**
    - For any valid share parameters, repeating the same create call yields the same share ID and never a duplicate active grant
    - NOT DONE: `TestCreateIdempotent_*` are table-driven examples, not generative property tests
    - **Validates: Requirements 12.1**

  - [x]* 5.5 Write unit tests for authorization client
    - Test CRUD operations against mocked Authorization Service
    - Test state-conflict detection and error messages
    - Test idempotency set-matching logic
    - _Requirements: 1.5, 3.1, 3.3, 3.5, 3.6, 9.1, 9.2, 12.1_

- [x] 6. VPC Lattice / RAM connectivity verification
  - [x] 6.1 Implement RAM association check
    - Query AWS RAM for resource share associations targeting the researcher account
    - Verify VPC Lattice service network association is in active/associated state
    - If missing or inactive: refuse share creation with descriptive message
    - _Requirements: 5.1, 5.2, 5.3, 5.4_

  - [x] 6.2 Implement connectivity preflight in share flow
    - Run RAM/Lattice check as first step before any Athena query
    - On API unavailability or permission error: refuse and report reason
    - _Requirements: 5.4, 5.5_

  - [x]* 6.3 Write unit tests for RAM association verification
    - Test active, inactive, and missing association states
    - Test API unavailability handling
    - _Requirements: 5.3, 5.4, 5.5_

- [ ] 7. Manifest writer
  - [x] 7.1 Implement manifest generation logic
    - Build Manifest JSON from active Share_Records for a researcher account
    - Group by library; compute effective expiry per library (latest across shares, null if any unlimited)
    - Construct htsget request paths (reads/<id>) with optional Access_Rules
    - Include htsgetBaseUrl and generatedAt timestamp
    - _Requirements: 7.1, 7.5, 7.6, 7.7_

  - [x] 7.2 Implement manifest S3 upload and deletion
    - PutObject to well-known key in researcher's S3 bucket using Data Control credentials
    - Delete manifest when no active shares remain for the account
    - Handle cross-account write (relies on researcher bucket policy grant)
    - SUPERSEDED: the manifest bucket is now a same-account Data Control bucket (Req 7.9); task 20.1 redirects this write to the configured Data Control bucket with a per-account key
    - _Requirements: 7.2, 7.3, 7.4, 11.5_

  - [ ]* 7.3 Write property test for manifest convergence (Property 9)
    - **Property 9: Manifest convergence**
    - After any sequence of create/revoke/extend operations, the manifest reflects exactly the remaining active grants and is deleted when none remain
    - NOT DONE: no generative test over operation sequences exists
    - **Validates: Requirements 7.3, 7.4**

  - [x]* 7.4 Write unit tests for manifest generation
    - Test JSON schema conformance
    - Test effective expiry computation (multiple shares, unlimited shares)
    - Test empty-share deletion behavior
    - _Requirements: 7.6, 7.7, 7.4_

- [x] 8. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 9. Share command orchestration
  - [x] 9.1 Wire the `share` command end-to-end
    - Input validation: reject empty library list, missing account, expiry < 1 day
    - Orchestrate: preflight (Lattice/RAM) → resolve (Athena) → idempotency check → create Share_Record → generate and upload manifest
    - Output confirmation: share ID, library count, expiry
    - Exit non-zero on any failure without partial state
    - _Requirements: 1.2, 1.5, 1.8, 1.9, 2.1, 2.2, 5.4, 7.1, 12.1_

  - [ ]* 9.2 Write property test for no partial shares in share command (Property 4)
    - **Property 4: No partial shares (end-to-end)**
    - Inject failures at resolution, preflight, and auth-service stages; verify no Share_Record is persisted on any failure path
    - NOT DONE: `cmd/` has no test files at all
    - **Validates: Requirements 1.6, 1.7, 1.8**

- [ ] 10. Revoke command
  - [x] 10.1 Implement the `revoke` command
    - Accept share ID and optional `--flush-cache` flag
    - Mark share as revoked via auth service client
    - If `--flush-cache`: call API Gateway `FlushStageCache` for the affected researcher
    - Regenerate manifest (or delete if no active shares remain)
    - Report revocation timestamp on success
    - _Requirements: 3.1, 3.2, 3.4, 3.7, 3.8, 7.3, 7.4_

  - [ ]* 10.2 Write unit tests for revoke command
    - Test successful revocation flow
    - Test cache flush invocation
    - Test manifest regeneration after revoke
    - Test error cases (not found, already revoked, already expired)
    - NOT DONE: `cmd/` has no test files at all
    - _Requirements: 3.1, 3.3, 3.5, 3.6, 3.7_

- [ ] 11. Extend command
  - [x] 11.1 Implement the `extend` command
    - Accept share ID and new duration
    - Compute new expiry (now + duration), update Share_Record
    - Regenerate manifest for affected researcher account
    - Report new expiry on success
    - Handle unlimited shares per Req 9.5
    - _Requirements: 9.1, 9.2, 9.3, 9.4, 9.5_

  - [ ]* 11.2 Write unit tests for extend command
    - Test successful extension
    - Test error: revoked/expired share
    - Test error: share not found
    - Test: extending unlimited share behavior
    - NOT DONE: `cmd/` has no test files at all
    - _Requirements: 9.1, 9.2, 9.4, 9.5_

- [ ] 12. List command
  - [x] 12.1 Implement the `list` command
    - Query all Share_Records (or filtered by --account)
    - Display tabular output: share ID, account, library IDs, created, expiry, status
    - Order by creation date descending
    - Handle empty results with informative message and zero exit code
    - _Requirements: 8.1, 8.2, 8.3, 8.4, 8.5, 8.6_

  - [ ]* 12.2 Write unit tests for list command
    - Test tabular output formatting
    - Test account filter
    - Test empty result handling
    - NOT DONE: `cmd/` has no test files at all
    - _Requirements: 8.1, 8.2, 8.3, 8.4_

- [x] 13. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 14. Authorization Service (Lambda + DynamoDB)
  - [x] 14.1 Implement the authorization callout handler
    - Accept GET request with source VPC ARN
    - Query DynamoDB GSI1 (targetVpcArn) for active, non-expired, non-revoked shares
    - Compute effective status at read time (compare expiresAt/revokedAt to now)
    - Build htsgetAuth response: array of location entries with S3 prefixes
    - Return empty htsgetAuth array for no matches (deny)
    - _Requirements: 4.2, 4.3, 4.4, 2.4, 2.5, 2.6_

  - [x] 14.2 Implement Cache-Control header computation
    - For allow: max-age = min(remaining lifetime among matched shares), capped at ceiling
    - For unlimited shares: use ceiling as their contribution
    - For deny (empty result): short max-age (configurable, default 60s)
    - _Requirements: 13.1, 13.2_

  - [x] 14.3 Implement the admin API handlers (create/revoke/extend/list)
    - Create: persist ShareRecord to DynamoDB, return share ID
    - Revoke: conditional update (revokedAt), state-conflict guards
    - Extend: conditional update (expiresAt), state-conflict guards
    - List: query GSI2 by researcherAccountId or scan
    - _Requirements: 10.2, 10.3, 10.6, 10.7_

  - [ ]* 14.4 Write property test for fail-closed authorization (Property 1)
    - **Property 1: Fail-closed**
    - For any callout input (including malformed, missing, or error states), verify the response is never an allow unless a valid active Share_Record authorizes it
    - NOT DONE: no generative test exists; this is the highest-priority missing property test
    - **Validates: Requirements 4.5, 4.6, 13.10**

  - [ ]* 14.5 Write property test for per-researcher isolation (Property 2)
    - **Property 2: Per-researcher isolation**
    - For any set of Share_Records and any two distinct requester VPC ARNs, verify the callout response for one never includes locations from the other's shares
    - NOT DONE: no generative test exists
    - **Validates: Requirements 13.3, 13.9**

  - [x]* 14.6 Write unit tests for authorization callout handler
    - Test allow response with valid active shares
    - Test deny on no matching shares
    - Test deny on expired/revoked shares
    - Test Cache-Control header values
    - Test DynamoDB unreachable → deny with error indication
    - _Requirements: 4.2, 4.3, 4.4, 10.4, 10.7, 13.1, 13.2_

- [ ] 15. Htsget-rs callout caching extension (Rust, upstream contribution)
  - The prototype in this repo's `htsget-cache/` crate is a standalone implementation with its own duplicated model and config types. It is **not** integrated into htsget-rs and therefore does not satisfy Requirement 14.7. The work below ports it into a clone of `umccr/htsget-rs` (sub-cloned into `htsget-rs/`, git-ignored) as an upstream pull request. See "Htsget_Rs integration" in design.md for the integration points.
  - [x] 15.1 Prototype the cache in a standalone crate (`htsget-cache/`)
    - Thread-safe TTL cache honoring `Cache-Control: max-age` and `Age`, TTL ceiling cap, missing `Cache-Control` treated as non-cacheable, per-identity keying, concurrency-safe
    - 59 unit tests plus a doc test pass under `cargo test`
    - _Requirements: 14.1, 14.2, 14.4, 14.5, 14.6_

  - [ ] 15.2 Clone and orient in the upstream repository
    - DONE: `umccr/htsget-rs` is sub-cloned at `htsget-rs/` (full history, so a branch can be pushed to a fork) and git-ignored
    - Confirm the current callout path: `htsget-config/src/config/advanced/callout.rs`, `.../auth/authorization.rs`, `.../auth/response.rs`, and `htsget-http/src/middleware/auth.rs`
    - Baseline `cargo test --all-features` and `cargo clippy` before changing anything
    - Open an upstream issue covering the change, including whether a new TTL-cache dependency (`moka`) is acceptable or whether the fallback (`RwLock<HashMap>` with manual TTL) is preferred
    - _Requirements: 14.7_

  - [ ] 15.3 Expose response headers from the callout fetch
    - `Auth::fetch_from_callout` currently consumes the body via `response.json()` and discards headers, so `Cache-Control`/`Age` are unavailable
    - Read `response.headers()` before consuming the body; surface them to the caller via a private helper without changing the existing public signature
    - _Requirements: 14.2_

  - [ ] 15.4 Add cache configuration to the upstream `Callout` type
    - Add `cache = { enabled, max_ttl }` to `Callout` in `htsget-config/src/config/advanced/callout.rs`, so caching is available to both auth and url-storage callouts
    - Match upstream conventions: `Serialize`/`Deserialize`/`JsonSchema`, `deny_unknown_fields`, defaults, 2-space `rustfmt`
    - Document the options in `htsget-config/README.md` and `htsget-config/docs/examples/auth.toml`
    - _Requirements: 14.3_

  - [ ] 15.5 Integrate the cache into the authorization callout path
    - Wrap `Auth::query_authorization_service` so a fresh callout is only issued on miss or expiry
    - Store the upstream `AuthorizationRestrictions` type; delete the duplicated `AuthorizationResponse`/`LocationEntry` model from the prototype
    - Key the cache on the callout URL plus the forwarded header set produced by `Auth::forwarded_headers` — deployment-neutral, and it carries the requester identity, preserving per-identity isolation without AWS-specific semantics
    - Cap TTL at the configured ceiling; treat a missing `Cache-Control` as non-cacheable
    - _Requirements: 14.1, 14.2, 14.4, 14.5, 14.6_

  - [ ]* 15.6 Write property test for caches never widen access (Property 3)
    - **Property 3: Caches never widen access**
    - For any sequence of cache operations, verify a cached entry never turns a deny into an allow; correctness holds with cache removed
    - **Validates: Requirements 13.10**

  - [ ] 15.7 Port the prototype test suite into the upstream crate
    - Move the `max-age`/`Age`, TTL-cap, miss-refetch, per-identity isolation, concurrency, and disabled-passthrough tests into the upstream crate's `#[cfg(test)] mod tests`
    - Add an integration test exercising the cached callout end to end through the auth middleware
    - `cargo test --all-features` and `cargo clippy` clean
    - _Requirements: 14.1, 14.2, 14.3, 14.4, 14.5, 14.6, 14.7_

  - [ ] 15.8 Raise the pull request and retire the prototype
    - Open the PR against `umccr/htsget-rs` following `CONTRIBUTING.md`
    - Once merged, delete `htsget-cache/` from this repo and record the PR reference in the documentation (Req 18.9)
    - _Requirements: 14.7, 18.9_

- [x] 16. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 17. Validate command
  - [x] 17.1 Implement provider-side validation checks
    - Credential resolution check (attempt an API call)
    - OrcaVault Mart reachability (probe query: `SELECT 1 FROM mart.bam LIMIT 1`)
    - Authorization Service reachability and schema conformance
    - Htsget_Rs health/service-info endpoint check
    - Cross-account S3 HeadObject on canary object (Data Account)
    - Report per-check pass/fail; continue on failure (don't abort on first)
    - _Requirements: 15.1, 15.2, 15.7_

  - [x] 17.2 Implement researcher-side validation checks
    - VPC Lattice service network association check for target account
    - AWS RAM resource share accepted status
    - Test_Lambda discovery and invocation (if deployed)
    - Report unavailable probe if Test_Lambda not found
    - _Requirements: 15.3, 15.4, 15.5_

  - [x] 17.3 Implement test-setup canary mode
    - Create short-lived synthetic Share_Record for configured canary library
    - Invoke Test_Lambda to confirm canary data retrievable end-to-end
    - Clean up synthetic share on exit (including on error, best-effort)
    - Report success/failure of simulated share path
    - _Requirements: 15.6, 15.8_

  - [ ]* 17.4 Write unit tests for validate command
    - Test each check independently (mock AWS clients)
    - Test continue-on-failure behavior (all checks run even if some fail)
    - Test canary cleanup on error
    - NOT DONE: `internal/validate/` has no test files at all
    - _Requirements: 15.1, 15.7, 15.8_

- [ ] 18. Integration wiring and end-to-end flow
  - [x] 18.1 Wire all commands to shared client factory and config
    - Connect each subcommand to the credential resolver, config loader, and service clients
    - Ensure consistent error formatting (stderr for errors, stdout for results)
    - Ensure non-zero exit codes on all error paths
    - _Requirements: 1.8, 11.6_

  - [x] 18.2 Implement cache flush on revoke (API Gateway FlushStageCache)
    - Call API Gateway management API to flush stage cache for the affected researcher
    - Best-effort: report but don't fail the revoke if flush fails
    - _Requirements: 3.8_

  - [ ]* 18.3 Write integration tests for share → revoke → manifest lifecycle
    - Test full lifecycle: share creates record + manifest, revoke updates both, manifest deleted when empty
    - Use mocked AWS services (DynamoDB Local, S3 emulator)
    - NOT DONE: no integration test exists
    - _Requirements: 1.5, 3.1, 7.1, 7.3, 7.4_

- [x] 19. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 20. Manifest bucket configuration and preflight
  - [ ] 20.1 Make the Manifest_Bucket resolution configuration-driven
    - Add a `manifest` config section: `bucket` (the Data Control Manifest_Bucket, sourced from the Infrastructure_Stack output) and `keyTemplate` (default `manifests/{accountId}/manifest.json`)
    - Replace the hard-coded `researcherBucketName` helper in `cmd/share.go` with a single-bucket + per-account key resolution from config; use it consistently in `share`, `revoke`, and `extend`
    - Validate the key template renders and rejects unknown placeholders
    - _Requirements: 7.2, 7.9_

  - [ ] 20.2 Add the Manifest_Bucket check to share preflight
    - Verify the configured bucket exists and is writable by the Data Control identity (same-account `HeadBucket`/put) before any Share_Record is created
    - On failure, report the expected bucket name and that the Infrastructure_Stack is not deployed or is misconfigured
    - Removes the window where a share is persisted and only then fails on manifest upload
    - _Requirements: 7.10, 17.7, 1.8_

  - [ ] 20.3 Extend the `validate` command with the new checks
    - Provider-side: the DynamoDB table, the Manifest_Bucket (present and writable), and the configured API Gateway stage exist, reporting which deployment step is missing (Req 15.10)
    - Researcher-side (with a target account): the Manifest_Bucket policy grants that account read on its own prefix, reporting when the onboarding grant is missing (Req 15.9)
    - _Requirements: 15.9, 15.10_

  - [ ]* 20.4 Write unit tests for bucket/key resolution and preflight
    - Key-template rendering, per-account key derivation, invalid template rejection
    - Preflight failure aborts before any Share_Record write
    - _Requirements: 7.2, 7.10_

- [ ] 21. Authorization Service Lambda entrypoints
  - [ ] 21.1 Implement the authorization callout entrypoint
    - `functions/authorization/main.go`: `lambda.Start` wiring for the existing `internal/auth` callout handler
    - Read table name, TTL ceiling, and deny TTL from environment; fail fast on missing configuration
    - Emit the `Cache-Control` header computed by the existing handler on the API Gateway proxy response
    - _Requirements: 16.6, 4.2, 4.3, 13.1, 13.2_

  - [ ] 21.2 Implement the admin entrypoint
    - `functions/admin/main.go`: `lambda.Start` wiring for the existing `internal/auth` admin handlers, routing create/revoke/extend/list
    - Map handler state-conflict results onto the HTTP status codes the CLI client expects
    - _Requirements: 16.6, 10.3, 10.6, 10.7_

  - [ ] 21.3 Keep entrypoints as wiring only
    - No business logic in `functions/`; both entrypoints delegate to `internal/auth` so the handler logic has one implementation shared with existing tests
    - Verify with `go build ./...` and confirm both binaries cross-compile for the Lambda runtime
    - _Requirements: 16.6_

- [ ] 22. Infrastructure_Stack (CDK)
  - [ ] 22.1 Scaffold the CDK application
    - Create `infrastructure/` as a CDK TypeScript app, consistent with the surrounding UMCCR deployments
    - Parameterise account, region, resource naming, and the set of researcher accounts granted manifest read, so one source deploys to multiple environments
    - _Requirements: 16.1, 16.10_

  - [ ] 22.2 Provision the Share_Record table
    - DynamoDB table with PK `shareId`, GSI1 PK `targetVpcArn`, GSI2 PK `researcherAccountId` SK `createdAt`
    - Explicitly no TTL: expired and revoked records must remain visible to `list`
    - Project onto GSI1 only the attributes the callout handler reads
    - _Requirements: 16.2, 10.2, 8.2_

  - [ ] 22.3 Provision the Lambda functions and their roles
    - Deploy `functions/authorization` and `functions/admin`
    - Execution roles scoped to the table and only the indexes each function queries
    - Pass table name and cache TTL settings via environment
    - _Requirements: 16.6, 16.7_

  - [ ] 22.4 Provision the API Gateway REST API and response cache
    - REST API (not HTTP API — HTTP APIs provide neither response caching nor `FlushStageCache`)
    - Enable stage response caching; include the forwarded requester identity in the cache key so no response crosses researchers
    - Align cache TTL with the emitted `max-age`, capped at the ceiling
    - _Requirements: 16.3, 13.3, 13.4, 3.8_

  - [ ] 22.5 Provision the Manifest_Bucket and its cross-account read policy
    - Single private bucket, block public access, per-account key prefix (`manifests/<account-id>/manifest.json`)
    - Bucket policy grants each researcher account (from the stack parameter) `s3:GetObject` on its own prefix only; matching KMS key-policy grant if SSE-KMS
    - The Lambda/admin path does not touch this bucket; only the CLI (via operator credentials) writes it
    - _Requirements: 16.5, 7.9, 11.5_

  - [ ] 22.6 Export stack outputs and optionally the operator policy
    - Export the REST API id, stage name, and Manifest_Bucket name — the `endpoints.apiGatewayRestApiId`, `endpoints.apiGatewayStageName`, and `manifest.bucket` config values
    - Optionally provision the operator IAM policy of Req 11.2 (now including same-account manifest write); the CLI still creates no IAM at runtime
    - _Requirements: 16.4, 16.9, 11.2, 11.3_

  - [ ]* 22.7 Write CDK assertion tests on the synthesised template
    - Table has both GSIs and no TTL; API is a REST API with caching enabled and identity in the cache key
    - Manifest_Bucket blocks public access and its policy grants only per-prefix read to the configured accounts
    - Execution roles grant no more than the handlers need; outputs are present
    - _Requirements: 16.2, 16.3, 16.4, 16.5, 16.7_

- [ ] 23. Close the outstanding test gaps
  - [ ] 23.1 Introduce a property-testing library
    - Add a generative testing dependency (for example `pgregory.net/rapid`) and a shared generator package for Share_Records, VPC ARNs, and operation sequences
    - _Requirements: 4.5, 12.1, 13.9_

  - [ ] 23.2 Implement the missing property tests
    - Properties 1 and 2 first (fail-closed and per-researcher isolation — the security-sensitive ones), then 3, 4, 9, 10
    - Tasks 3.4, 5.4, 7.3, 9.2, 14.4, 14.5, 15.6 are satisfied by this work
    - _Requirements: 1.6, 1.7, 1.8, 4.5, 4.6, 7.3, 7.4, 12.1, 13.3, 13.9, 13.10_

  - [ ] 23.3 Implement the missing command and validate tests
    - Unit tests for `cmd/` (share, revoke, extend, list) and `internal/validate/`, which currently have no test files
    - Tasks 10.2, 11.2, 12.2, 17.4 are satisfied by this work
    - _Requirements: 3.1, 8.1, 9.1, 15.1, 15.7, 15.8_

  - [ ] 23.4 Implement the share → revoke → manifest integration test
    - Full lifecycle against DynamoDB Local and an S3 emulator, including manifest deletion when no active shares remain
    - Task 18.3 is satisfied by this work
    - _Requirements: 1.5, 3.1, 7.1, 7.3, 7.4_

- [ ] 24. Documentation
  - [ ] 24.1 Write the README and usage documentation
    - README: what the tool does, the account topology, and how the CLI, Authorization_Service, Htsget_Rs, and OrcaVault Mart relate
    - `docs/usage.md`: every subcommand, its flags, worked examples with representative output, and the exit-code convention
    - _Requirements: 18.1, 18.2_

  - [ ] 24.2 Write the configuration and deployment documentation
    - `docs/configuration.md`: every field, required vs optional, defaults, flag precedence, config search paths, annotated example
    - `docs/deployment.md`: prerequisites, CDK synth/deploy commands, targeting an account and region, stack outputs and how they map onto config fields
    - _Requirements: 18.3, 18.4_

  - [ ] 24.3 Write the prerequisites and onboarding documentation
    - `docs/prerequisites.md`: per-account checklist derived from the design's deployment ownership table, stating the owning project and how to verify each item
    - `docs/onboarding.md`: Researcher_Account onboarding end to end — RAM acceptance, Lattice association, adding the account to the Manifest_Bucket cross-account read policy (Data Control side) and the researcher's own IAM read permission, optional Test_Lambda, ending in a `validate` run
    - _Requirements: 18.5, 18.6, 17.1_

  - [ ] 24.4 Write the operational and manifest documentation
    - `docs/operations.md`: Cache_TTL-bounded revocation and what `--flush-cache` does and does not do, immutable library sets and revoke-and-recreate, idempotent creation, and the ~1 day OrcaVault lag
    - `docs/manifest.md`: manifest schema, location, and that it is descriptive only and never consulted for authorization
    - Record the htsget-rs contribution reference from task 15
    - _Requirements: 18.7, 18.8, 18.9_

- [ ] 25. Final checkpoint - Ensure all tests pass
  - Go tests, CDK assertion tests, and the upstream htsget-rs test suite all pass; documentation matches implemented behaviour (Req 18.10). Ask the user if questions arise.

## Notes

- Tasks marked with `*` are optional and can be skipped for faster MVP
- Each task references specific requirements for traceability
- Checkpoints ensure incremental validation
- Property tests validate universal correctness properties from the design document
- Unit tests validate specific examples and edge cases
- Tasks 3.4, 5.4, 7.3, 9.2, 10.2, 11.2, 12.2, 14.4, 14.5, 17.4, and 18.3 were previously marked complete but have no corresponding tests in the tree. They are unchecked and consolidated under task 23; the `NOT DONE` note on each records why
- Task 15 is an upstream contribution to `umccr/htsget-rs`, worked in a git-ignored sub-clone at `htsget-rs/`. The `htsget-cache/` crate in this repo is a prototype only and is deleted once the PR lands. Task 15 can proceed in parallel with everything else
- Tasks 21 and 22 are ordered: the CDK stack cannot deploy Lambda functions whose `main` packages do not yet exist
- The Authorization Service handlers already exist in `internal/auth`; task 21 adds only the deployable entrypoints around them

## Task Dependency Graph

```json
{
  "waves": [
    { "id": 0, "tasks": ["1.1"] },
    { "id": 1, "tasks": ["1.2", "1.3"] },
    { "id": 2, "tasks": ["1.4", "2.1"] },
    { "id": 3, "tasks": ["2.2", "3.1", "5.1", "6.1", "7.1", "15.1"] },
    { "id": 4, "tasks": ["3.2", "5.2", "5.3", "6.2", "7.2", "15.2"] },
    { "id": 5, "tasks": ["3.3", "3.5", "5.5", "6.3", "7.4", "15.3", "15.4"] },
    { "id": 6, "tasks": ["9.1", "10.1", "11.1", "12.1", "14.1", "15.5"] },
    { "id": 7, "tasks": ["14.2", "14.3", "15.7"] },
    { "id": 8, "tasks": ["14.6", "17.1", "15.8"] },
    { "id": 9, "tasks": ["17.2", "17.3", "18.1", "18.2"] },
    { "id": 10, "tasks": ["20.1", "21.1", "21.2"] },
    { "id": 11, "tasks": ["20.2", "20.3", "21.3", "22.1"] },
    { "id": 12, "tasks": ["20.4", "22.2", "22.3"] },
    { "id": 13, "tasks": ["22.4", "22.5", "23.1"] },
    { "id": 14, "tasks": ["22.6", "22.7", "23.2", "23.3", "23.4", "15.6"] },
    { "id": 15, "tasks": ["24.1", "24.2", "24.3", "24.4"] }
  ]
}
```
