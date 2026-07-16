# Implementation Plan: Data Releaser CLI

## Overview

The Data Releaser is a Go CLI tool that grants external researchers time-bounded (or unlimited) access to bioinformatics libraries via htsget-rs. Implementation proceeds from foundational types and configuration, through core subsystem clients (warehouse resolution, authorization service, VPC Lattice/RAM, manifest writer), to command orchestration, wiring, and validation. The htsget-rs callout caching extension (Requirement 14) is a separate Rust deliverable in the UMCCR htsget-rs repo.

## Tasks

- [ ] 1. Project structure, configuration, and core types
  - [ ] 1.1 Initialize Go module, directory layout, and CLI skeleton
    - Create `go.mod`, top-level `main.go`, and package directories: `cmd/`, `internal/config/`, `internal/warehouse/`, `internal/auth/`, `internal/lattice/`, `internal/manifest/`, `internal/validate/`, `internal/model/`
    - Set up Cobra (or similar) CLI framework with stub subcommands: `share`, `revoke`, `extend`, `list`, `validate`
    - _Requirements: 1, 3, 8, 9, 15_

  - [ ] 1.2 Define core domain types and data models
    - Implement `ShareRecord` struct (shareId ULID, researcherAccountId, targetVpcArn, libraryIds, s3Prefixes, createdAt, expiresAt, revokedAt, status)
    - Implement effective-status derivation logic (active/expired/revoked based on current time)
    - Implement `AccessRule` sub-schema struct (referenceName, format, start, end)
    - Implement `Manifest` struct (htsgetBaseUrl, generatedAt, libraries with files and rules)
    - Implement `ResolvedLibrary` struct (libraryId, portalRunId, bucket, key, format, storageClass)
    - _Requirements: 1.5, 2.3, 7.6, 10.2_

  - [ ] 1.3 Implement configuration loading (YAML + flags)
    - Parse config file (profile, warehouse settings, endpoints, cache TTLs, validation canary)
    - Support CLI flag overrides for profile, account, expiry, etc.
    - Validate required fields; reject invalid config early
    - _Requirements: 11.1, 11.3, 13.1, 13.2_

  - [ ]* 1.4 Write unit tests for effective-status derivation and config validation
    - Test active/expired/revoked status computation at boundary times
    - Test config parsing with valid, missing, and invalid inputs
    - _Requirements: 2.3, 2.4, 11.1_

- [ ] 2. AWS credential resolution and SDK client factory
  - [ ] 2.1 Implement credential resolution via standard AWS SDK chain
    - Use AWS SDK for Go v2 `config.LoadDefaultConfig` with optional profile override from config
    - Wrap in a factory that returns configured SDK clients (Athena, DynamoDB, RAM, S3, Lambda, API Gateway)
    - Report clear error on credential failure and exit non-zero without partial actions
    - _Requirements: 11.1, 11.2, 11.3, 11.6_

  - [ ]* 2.2 Write unit tests for credential error handling
    - Mock credential resolution failure and verify non-zero exit with descriptive error
    - _Requirements: 11.6_

- [ ] 3. OrcaVault warehouse resolution client
  - [ ] 3.1 Implement Athena query builder for library resolution
    - Build parameterized SQL query against `mart.bam` only (no `mart.lims`, no `mart.output`), per design
    - Filter `mart.bam` to `format = 'bam'` only
    - Ensure library IDs are parameterized/validated (no string concatenation) to prevent SQL injection
    - _Requirements: 1.1, 1.3, 1.4_

  - [ ] 3.2 Implement Athena query execution and polling
    - Call `StartQueryExecution` with configured workgroup/database/catalog
    - Poll `GetQueryExecution` until SUCCEEDED or FAILED/CANCELLED
    - On non-success terminal state, report the Athena error and abort
    - Parse `GetQueryResults` into `ResolvedLibrary` structs
    - _Requirements: 1.3, 1.8_

  - [ ] 3.3 Implement library resolution and prefix derivation
    - Report a library ID with no `format = 'bam'` rows in `mart.bam` as having no shareable reads (message: the ID may not exist, may not be aligned yet, or its reads may be archived)
    - Derive S3 prefix as the parent prefix of the resolved `.bam` `key` (covers the co-located `.bai` index)
    - Detect archived `storage_class` (DeepArchive, Glacier tiers) and abort if found
    - Collect all errors before aborting (report all no-shareable-reads and archived library IDs at once)
    - _Requirements: 1.1, 1.4, 1.6, 1.7_

  - [ ]* 3.4 Write property test for warehouse resolution (Property 4: No partial shares)
    - **Property 4: No partial shares**
    - For any combination of no-shareable-reads and archived libraries in the input, verify that no Share_Record is created and all problematic IDs are reported
    - **Validates: Requirements 1.6, 1.7, 1.8**

  - [ ]* 3.5 Write unit tests for Athena query builder and result parsing
    - Test SQL generation with varying library ID counts
    - Test prefix derivation from the `.bam` key
    - Test archived storage_class detection
    - Test no-shareable-reads detection (library ID with no `format = 'bam'` rows)
    - _Requirements: 1.1, 1.3, 1.4, 1.6, 1.7_

- [ ] 4. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 5. Authorization Service client (admin interface)
  - [ ] 5.1 Implement Share_Record CRUD operations
    - Create: persist ShareRecord with ULID, computed expiry, s3Prefixes, status=active
    - Revoke: set revokedAt timestamp, verify share is active (not already revoked/expired)
    - Extend: update expiresAt to now + duration, verify share is active
    - List: query by researcherAccountId (GSI2) or scan all, ordered by createdAt desc
    - _Requirements: 1.5, 3.1, 9.1, 8.1_

  - [ ] 5.2 Implement idempotency check for share creation
    - Query existing active shares for same researcherAccountId (GSI2)
    - Compare library ID sets (order-independent) for exact match
    - If match found: return existing share ID without creating duplicate
    - If partial overlap: proceed with new share creation
    - _Requirements: 12.1, 12.2, 12.3, 12.4_

  - [ ] 5.3 Implement state-conflict error handling
    - Revoke on already-revoked: report specific error
    - Revoke on expired: report specific error
    - Extend on revoked/expired: report specific error
    - Unknown share ID: report not-found error
    - All leave state unchanged on conflict
    - _Requirements: 3.3, 3.5, 3.6, 9.2, 9.4_

  - [ ]* 5.4 Write property test for idempotent creation (Property 10)
    - **Property 10: Idempotent creation**
    - For any valid share parameters, repeating the same create call yields the same share ID and never a duplicate active grant
    - **Validates: Requirements 12.1**

  - [ ]* 5.5 Write unit tests for authorization client
    - Test CRUD operations against mocked Authorization Service
    - Test state-conflict detection and error messages
    - Test idempotency set-matching logic
    - _Requirements: 1.5, 3.1, 3.3, 3.5, 3.6, 9.1, 9.2, 12.1_

- [ ] 6. VPC Lattice / RAM connectivity verification
  - [ ] 6.1 Implement RAM association check
    - Query AWS RAM for resource share associations targeting the researcher account
    - Verify VPC Lattice service network association is in active/associated state
    - If missing or inactive: refuse share creation with descriptive message
    - _Requirements: 5.1, 5.2, 5.3, 5.4_

  - [ ] 6.2 Implement connectivity preflight in share flow
    - Run RAM/Lattice check as first step before any Athena query
    - On API unavailability or permission error: refuse and report reason
    - _Requirements: 5.4, 5.5_

  - [ ]* 6.3 Write unit tests for RAM association verification
    - Test active, inactive, and missing association states
    - Test API unavailability handling
    - _Requirements: 5.3, 5.4, 5.5_

- [ ] 7. Manifest writer
  - [ ] 7.1 Implement manifest generation logic
    - Build Manifest JSON from active Share_Records for a researcher account
    - Group by library; compute effective expiry per library (latest across shares, null if any unlimited)
    - Construct htsget request paths (reads/<id>) with optional Access_Rules
    - Include htsgetBaseUrl and generatedAt timestamp
    - _Requirements: 7.1, 7.5, 7.6, 7.7_

  - [ ] 7.2 Implement manifest S3 upload and deletion
    - PutObject to well-known key in researcher's S3 bucket using Data Control credentials
    - Delete manifest when no active shares remain for the account
    - Handle cross-account write (relies on researcher bucket policy grant)
    - _Requirements: 7.2, 7.3, 7.4, 11.5_

  - [ ]* 7.3 Write property test for manifest convergence (Property 9)
    - **Property 9: Manifest convergence**
    - After any sequence of create/revoke/extend operations, the manifest reflects exactly the remaining active grants and is deleted when none remain
    - **Validates: Requirements 7.3, 7.4**

  - [ ]* 7.4 Write unit tests for manifest generation
    - Test JSON schema conformance
    - Test effective expiry computation (multiple shares, unlimited shares)
    - Test empty-share deletion behavior
    - _Requirements: 7.6, 7.7, 7.4_

- [ ] 8. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 9. Share command orchestration
  - [ ] 9.1 Wire the `share` command end-to-end
    - Input validation: reject empty library list, missing account, expiry < 1 day
    - Orchestrate: preflight (Lattice/RAM) → resolve (Athena) → idempotency check → create Share_Record → generate and upload manifest
    - Output confirmation: share ID, library count, expiry
    - Exit non-zero on any failure without partial state
    - _Requirements: 1.2, 1.5, 1.8, 1.9, 2.1, 2.2, 5.4, 7.1, 12.1_

  - [ ]* 9.2 Write property test for no partial shares in share command (Property 4)
    - **Property 4: No partial shares (end-to-end)**
    - Inject failures at resolution, preflight, and auth-service stages; verify no Share_Record is persisted on any failure path
    - **Validates: Requirements 1.6, 1.7, 1.8**

- [ ] 10. Revoke command
  - [ ] 10.1 Implement the `revoke` command
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
    - _Requirements: 3.1, 3.3, 3.5, 3.6, 3.7_

- [ ] 11. Extend command
  - [ ] 11.1 Implement the `extend` command
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
    - _Requirements: 9.1, 9.2, 9.4, 9.5_

- [ ] 12. List command
  - [ ] 12.1 Implement the `list` command
    - Query all Share_Records (or filtered by --account)
    - Display tabular output: share ID, account, library IDs, created, expiry, status
    - Order by creation date descending
    - Handle empty results with informative message and zero exit code
    - _Requirements: 8.1, 8.2, 8.3, 8.4, 8.5, 8.6_

  - [ ]* 12.2 Write unit tests for list command
    - Test tabular output formatting
    - Test account filter
    - Test empty result handling
    - _Requirements: 8.1, 8.2, 8.3, 8.4_

- [ ] 13. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 14. Authorization Service (Lambda + DynamoDB)
  - [ ] 14.1 Implement the authorization callout handler
    - Accept GET request with source VPC ARN
    - Query DynamoDB GSI1 (targetVpcArn) for active, non-expired, non-revoked shares
    - Compute effective status at read time (compare expiresAt/revokedAt to now)
    - Build htsgetAuth response: array of location entries with S3 prefixes
    - Return empty htsgetAuth array for no matches (deny)
    - _Requirements: 4.2, 4.3, 4.4, 2.4, 2.5, 2.6_

  - [ ] 14.2 Implement Cache-Control header computation
    - For allow: max-age = min(remaining lifetime among matched shares), capped at ceiling
    - For unlimited shares: use ceiling as their contribution
    - For deny (empty result): short max-age (configurable, default 60s)
    - _Requirements: 13.1, 13.2_

  - [ ] 14.3 Implement the admin API handlers (create/revoke/extend/list)
    - Create: persist ShareRecord to DynamoDB, return share ID
    - Revoke: conditional update (revokedAt), state-conflict guards
    - Extend: conditional update (expiresAt), state-conflict guards
    - List: query GSI2 by researcherAccountId or scan
    - _Requirements: 10.2, 10.3, 10.6, 10.7_

  - [ ]* 14.4 Write property test for fail-closed authorization (Property 1)
    - **Property 1: Fail-closed**
    - For any callout input (including malformed, missing, or error states), verify the response is never an allow unless a valid active Share_Record authorizes it
    - **Validates: Requirements 4.5, 4.6, 13.10**

  - [ ]* 14.5 Write property test for per-researcher isolation (Property 2)
    - **Property 2: Per-researcher isolation**
    - For any set of Share_Records and any two distinct requester VPC ARNs, verify the callout response for one never includes locations from the other's shares
    - **Validates: Requirements 13.3, 13.9**

  - [ ]* 14.6 Write unit tests for authorization callout handler
    - Test allow response with valid active shares
    - Test deny on no matching shares
    - Test deny on expired/revoked shares
    - Test Cache-Control header values
    - Test DynamoDB unreachable → deny with error indication
    - _Requirements: 4.2, 4.3, 4.4, 10.4, 10.7, 13.1, 13.2_

- [ ] 15. Htsget-rs callout caching extension (Rust, separate repo)
  - [ ] 15.1 Implement in-process authorization response cache in htsget-rs
    - Add a thread-safe cache (e.g. `moka` or `mini-moka`) keyed on requester identity (source VPC ARN)
    - Honor `Cache-Control: max-age` from the Authorization_Service response
    - Respect `Age` header to compute remaining freshness
    - Cap TTL at configurable ceiling; treat missing Cache-Control as non-cacheable
    - On cache miss or expiry: issue fresh callout and populate cache
    - _Requirements: 14.1, 14.2, 14.4, 14.6_

  - [ ] 15.2 Extend htsget-rs callout configuration
    - Add config options: `cache.enabled` (bool), `cache.max_ttl` (duration ceiling)
    - Integrate with existing `[auth.authorization]` `kind = "callout"` config structure
    - _Requirements: 14.3_

  - [ ] 15.3 Ensure concurrency safety and per-identity isolation
    - Verify cache is safe for concurrent access (multiple requests in-flight)
    - Verify entries scoped to requester identity — never cross-requester reuse
    - _Requirements: 14.5_

  - [ ]* 15.4 Write property test for caches never widen access (Property 3)
    - **Property 3: Caches never widen access**
    - For any sequence of cache operations, verify a cached entry never turns a deny into an allow; correctness holds with cache removed
    - **Validates: Requirements 13.10**

  - [ ]* 15.5 Write unit and integration tests for callout cache
    - Test max-age honoring and Age header computation
    - Test TTL cap enforcement
    - Test cache miss → fresh callout
    - Test per-identity key isolation
    - Test concurrent access safety
    - Test disabled cache passthrough
    - _Requirements: 14.1, 14.2, 14.3, 14.4, 14.5, 14.6, 14.7_

- [ ] 16. Checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

- [ ] 17. Validate command
  - [ ] 17.1 Implement provider-side validation checks
    - Credential resolution check (attempt an API call)
    - OrcaVault Mart reachability (probe query: `SELECT 1 FROM mart.bam LIMIT 1`)
    - Authorization Service reachability and schema conformance
    - Htsget_Rs health/service-info endpoint check
    - Cross-account S3 HeadObject on canary object (Data Account)
    - Report per-check pass/fail; continue on failure (don't abort on first)
    - _Requirements: 15.1, 15.2, 15.7_

  - [ ] 17.2 Implement researcher-side validation checks
    - VPC Lattice service network association check for target account
    - AWS RAM resource share accepted status
    - Test_Lambda discovery and invocation (if deployed)
    - Report unavailable probe if Test_Lambda not found
    - _Requirements: 15.3, 15.4, 15.5_

  - [ ] 17.3 Implement test-setup canary mode
    - Create short-lived synthetic Share_Record for configured canary library
    - Invoke Test_Lambda to confirm canary data retrievable end-to-end
    - Clean up synthetic share on exit (including on error, best-effort)
    - Report success/failure of simulated share path
    - _Requirements: 15.6, 15.8_

  - [ ]* 17.4 Write unit tests for validate command
    - Test each check independently (mock AWS clients)
    - Test continue-on-failure behavior (all checks run even if some fail)
    - Test canary cleanup on error
    - _Requirements: 15.1, 15.7, 15.8_

- [ ] 18. Integration wiring and end-to-end flow
  - [ ] 18.1 Wire all commands to shared client factory and config
    - Connect each subcommand to the credential resolver, config loader, and service clients
    - Ensure consistent error formatting (stderr for errors, stdout for results)
    - Ensure non-zero exit codes on all error paths
    - _Requirements: 1.8, 11.6_

  - [ ] 18.2 Implement cache flush on revoke (API Gateway FlushStageCache)
    - Call API Gateway management API to flush stage cache for the affected researcher
    - Best-effort: report but don't fail the revoke if flush fails
    - _Requirements: 3.8_

  - [ ]* 18.3 Write integration tests for share → revoke → manifest lifecycle
    - Test full lifecycle: share creates record + manifest, revoke updates both, manifest deleted when empty
    - Use mocked AWS services (DynamoDB Local, S3 emulator)
    - _Requirements: 1.5, 3.1, 7.1, 7.3, 7.4_

- [ ] 19. Final checkpoint - Ensure all tests pass
  - Ensure all tests pass, ask the user if questions arise.

## Notes

- Tasks marked with `*` are optional and can be skipped for faster MVP
- Each task references specific requirements for traceability
- Checkpoints ensure incremental validation
- Property tests validate universal correctness properties from the design document
- Unit tests validate specific examples and edge cases
- Task 15 (htsget-rs caching) is a separate Rust deliverable in the UMCCR htsget-rs repository; it can proceed in parallel with the Go CLI tasks
- The Authorization Service (Task 14) can be implemented as a Go Lambda or a separate language; the design is language-agnostic for that component

## Task Dependency Graph

```json
{
  "waves": [
    { "id": 0, "tasks": ["1.1"] },
    { "id": 1, "tasks": ["1.2", "1.3"] },
    { "id": 2, "tasks": ["1.4", "2.1"] },
    { "id": 3, "tasks": ["2.2", "3.1", "5.1", "6.1", "7.1", "15.1"] },
    { "id": 4, "tasks": ["3.2", "5.2", "5.3", "6.2", "7.2", "15.2", "15.3"] },
    { "id": 5, "tasks": ["3.3", "3.4", "3.5", "5.4", "5.5", "6.3", "7.3", "7.4", "15.4", "15.5"] },
    { "id": 6, "tasks": ["9.1", "10.1", "11.1", "12.1", "14.1"] },
    { "id": 7, "tasks": ["9.2", "10.2", "11.2", "12.2", "14.2", "14.3"] },
    { "id": 8, "tasks": ["14.4", "14.5", "14.6", "17.1"] },
    { "id": 9, "tasks": ["17.2", "17.3", "18.1", "18.2"] },
    { "id": 10, "tasks": ["17.4", "18.3"] }
  ]
}
```
