# CLI usage

This document covers how to build and run the `data-releaser` CLI: every
subcommand, its flags, worked invocation examples with representative output,
and the exit-code convention.

## Building and running

```sh
# From the repository root — build a single binary
go build -o data-releaser .

# Run it
./data-releaser --help
```

> The whole module does not build with a bare `go build ./...` because
> `infrastructure/node_modules/` contains files the Go toolchain rejects. Scope
> the build to the Go packages instead:
> `go build ./cmd/... ./internal/... ./functions/... .`

You can also run without building an artifact:

```sh
go run . <command> [flags]
```

## Authentication

Every command runs under a single set of Data Control account credentials,
resolved via the standard AWS credential chain (a named profile backed by AWS
SSO / IAM Identity Center, a `credential_process` provider, environment
variables, or an assumed role). You never authenticate into more than one
account.

Select the source with the `profile` config field or the `--profile` flag;
omit both to use the default chain. See
[`docs/configuration.md`](configuration.md) for details.

## Global (persistent) flags

These flags are available on every subcommand:

| Flag | Description |
| --- | --- |
| `--config <path>` | Path to the config file. If omitted, the tool searches `./config.yaml` then `~/.config/data-releaser/config.yaml`. |
| `--profile <name>` | AWS profile override. Takes precedence over the `profile` field in the config file. |
| `-h`, `--help` | Help for the command. |

A valid config file must be resolvable for any command to run; if none is found,
the command fails before doing any work.

## Exit-code convention

The CLI follows a simple two-value convention:

- **`0`** — success. Note that some "empty" outcomes are still successes: for
  example, `list` finding no shares prints an informative message and exits `0`.
- **non-zero** (`1`) — failure. Any error — invalid input, a failed preflight,
  an unreachable service, a resolution problem, a state conflict, or one or more
  failed `validate` checks — is written to standard error prefixed with
  `Error:` and the process exits non-zero. On any failure the tool aborts
  without leaving partial state (for example, no partial Share_Record is ever
  created).

Results go to **standard output**; errors, warnings, and advisory notes go to
**standard error**.

---

## `share` — create a data share

Resolve library IDs to S3 locations, create a `Share_Record`, and deliver a
manifest to the researcher account.

```
data-releaser share --libraries <ids> --account <id> [--expiry <duration>]
```

| Flag | Required | Description |
| --- | --- | --- |
| `--libraries` | yes | Comma-separated list of library IDs to share (e.g. `L2400001,L2400002`). |
| `--account` | yes | Target researcher AWS account ID. |
| `--expiry` | no | Optional expiry duration. Minimum 1 day. If omitted, the share is unlimited (no expiry). |

**Duration format.** Durations accept a day suffix (`30d`, `7d`) or any standard
Go duration string (`24h`, `720h`, `48h`). A duration shorter than one day
(`24h`) is rejected.

**What it does, in order:**

1. Validates inputs (rejects an empty library list, a missing account, or an
   expiry below 1 day) before any network call.
2. Preflight: verifies the researcher account has an active VPC Lattice / RAM
   association; refuses if not.
3. Preflight: verifies the configured Manifest_Bucket exists and is writable by
   the Data Control identity, before any Share_Record is created.
4. Resolves the libraries with a single Athena query against `mart.bam`.
5. Validates resolution: aborts if any library has no shareable reads or reports
   an archived storage class (reporting all affected IDs at once).
6. Performs an idempotency check and creates the Share_Record (or reuses an
   existing identical active share).
7. Generates and uploads the manifest for the researcher.

**Example — create a 30-day share:**

```sh
$ data-releaser share --libraries L2400001,L2400002 --account 534840902377 --expiry 30d
Share created successfully:
  Share ID:  01JQ8XABCDEF0123456789ABCD
  Account:   534840902377
  Libraries: 2 (L2400001, L2400002)
  Expiry:    2026-08-10T04:00:00Z
```

**Example — unlimited share (no `--expiry`):**

```sh
$ data-releaser share --libraries L2400001 --account 534840902377
Share created successfully:
  Share ID:  01JQ8XABCDEF0123456789ABCD
  Account:   534840902377
  Libraries: 1 (L2400001)
  Expiry:    unlimited
```

**Example — idempotent re-run (same libraries + account as an active share):**

```sh
$ data-releaser share --libraries L2400001,L2400002 --account 534840902377 --expiry 30d
Existing share reused (idempotent match):
  Share ID:  01JQ8XABCDEF0123456789ABCD
  Account:   534840902377
  Libraries: 2 (L2400001, L2400002)
  Expiry:    2026-08-10T04:00:00Z
```

The existing share's identifier and expiry are returned unchanged; no duplicate
is created, and a differing `--expiry` does not modify the existing share. A
share whose library set only *partially* overlaps an existing share creates a
new share instead.

**Example — resolution problem (aborts, no partial share):**

```sh
$ data-releaser share --libraries L2400001,L9999999 --account 534840902377
Error: cannot share libraries:
  library ID L9999999 may not exist, may not be aligned yet, or may have reads that are archived
# exit status 1
```

An archived read reports similarly, naming the offending storage class, e.g.
`library ID L2400003 has archived storage class DeepArchive and cannot be
streamed by htsget-rs`.

---

## `revoke` — revoke an active share

Mark a Share_Record as revoked, immediately and durably.

```
data-releaser revoke <share-id>
```

| Argument / flag | Required | Description |
| --- | --- | --- |
| `<share-id>` | yes | The identifier of the share to revoke (positional). |

Revocation marks the record revoked and regenerates the researcher's manifest
(deleting it if no active shares remain for the account). Because authorization
decisions are cached by htsget-rs, a revocation takes full effect only once the
relevant cached decisions expire — bounded by the configured Cache-TTL ceiling.
There is no way to shorten that window for an individual revocation: the cache
holds no externally invalidatable entries, so the ceiling is the propagation
bound in every case.

**Example — revoke a share:**

```sh
$ data-releaser revoke 01JQ8XABCDEF0123456789ABCD
Share revoked successfully:
  Share ID:    01JQ8XABCDEF0123456789ABCD
  Revoked at:  2026-07-15T02:30:00Z
Note: propagation is bounded by the Cache-TTL ceiling.
```

**Error cases** (each exits non-zero, leaving state unchanged):

```sh
$ data-releaser revoke UNKNOWNID
Error: share "UNKNOWNID" not found

$ data-releaser revoke 01JQ8XABCDEF0123456789ABCD
Error: share "01JQ8XABCDEF0123456789ABCD" is already revoked

$ data-releaser revoke 01JQ8XABCDEF0123456789ABCD
Error: share "01JQ8XABCDEF0123456789ABCD" has already expired
```

> Library sets are immutable — there is no in-place edit of which libraries a
> share grants. To change a researcher's libraries, revoke the affected share
> and create a new one (revoke-and-recreate).

---

## `extend` — extend an active share's expiry

Update the expiry of an active Share_Record to the current time plus a new
duration, and regenerate the researcher's manifest.

```
data-releaser extend <share-id> --duration <duration>
```

| Argument / flag | Required | Description |
| --- | --- | --- |
| `<share-id>` | yes | The identifier of the share to extend (positional). |
| `--duration` | yes | New duration from now. Minimum 1 day. Same format as `share --expiry` (`30d`, `24h`, ...). |

The new expiry is computed as *now + duration* (it is not added to the existing
expiry).

**Example — extend by 60 days:**

```sh
$ data-releaser extend 01JQ8XABCDEF0123456789ABCD --duration 60d
Share extended successfully:
  Share ID:   01JQ8XABCDEF0123456789ABCD
  Account:    534840902377
  Libraries:  2 (L2400001, L2400002)
  New Expiry: 2026-09-13T02:30:00Z
```

**Example — extending an unlimited share:**

```sh
$ data-releaser extend 01JQ8XABCDEF0123456789ABCD --duration 60d
Share 01JQ8XABCDEF0123456789ABCD already has unlimited duration; no extension needed.
```

An unlimited share already grants access indefinitely, so no extension is
applied and the command exits `0`.

**Error cases** (each exits non-zero, leaving state unchanged):

```sh
$ data-releaser extend UNKNOWNID --duration 30d
Error: share "UNKNOWNID" not found

$ data-releaser extend 01JQ8XABCDEF0123456789ABCD --duration 30d
Error: share "01JQ8XABCDEF0123456789ABCD" has been revoked and cannot be extended

$ data-releaser extend 01JQ8XABCDEF0123456789ABCD --duration 30d
Error: share "01JQ8XABCDEF0123456789ABCD" has already expired and cannot be extended
```

---

## `list` — list shares

Query all Share_Records and display them in a tabular format, ordered by
creation date descending, optionally filtered by researcher account.

```
data-releaser list [--account <id>]
```

| Flag | Required | Description |
| --- | --- | --- |
| `--account` | no | Filter to shares for a single researcher AWS account ID. |

Every record is shown regardless of status, so expired and revoked shares remain
visible for audit. The status column shows the *effective* status computed at
read time (`active`, `expired`, or `revoked`).

**Example — list all shares:**

```sh
$ data-releaser list
SHARE ID                    ACCOUNT       LIBRARIES              CREATED               EXPIRY                STATUS
01JQ8XABCDEF0123456789ABCD  534840902377  L2400001,L2400002      2026-07-11T04:00:00Z  2026-08-10T04:00:00Z  active
01JQ7WZZZZ0000000000000000  534840902377  L2300009               2026-06-01T00:00:00Z  unlimited             active
01JQ6VYYYY0000000000000000  111111111111  L2200001,L2200002,...  2026-05-01T00:00:00Z  2026-05-15T00:00:00Z  expired
```

Libraries are truncated after three IDs (e.g. `L2200001,L2200002,L2200003 (+2
more)`); an unlimited share shows `unlimited` in the expiry column.

**Example — filter by account, no matches:**

```sh
$ data-releaser list --account 999999999999
No shares found for account 999999999999.
```

With no filter and no records at all, it prints `No shares found.` Both empty
cases exit `0`. If the Authorization Service is unreachable, the command reports
the failure to standard error and exits non-zero.

---

## `validate` — validate the sharing infrastructure

Run preflight and end-to-end checks on the deployed sharing infrastructure,
reporting a per-check pass/fail diagnostic. It is side-effect-free unless
`--test-setup` is given (which creates and then cleans up a synthetic canary
share).

```
data-releaser validate [--account <id>] [--test-setup]
```

| Flag | Required | Description |
| --- | --- | --- |
| `--account` | no | Target researcher AWS account ID; enables the researcher-side checks. |
| `--test-setup` | no | Run canary test-setup mode (creates a short-lived synthetic share, invokes the Test_Lambda, and cleans up). Requires a configured canary. |

**Provider-side checks** (always run):

- Credential Resolution (STS GetCallerIdentity)
- OrcaVault Mart Reachability (Athena) — a `SELECT 1 FROM mart.bam LIMIT 1` probe
- Authorization Service Reachability
- Htsget_Rs Health (service-info)
- Cross-Account S3 Access (Data Account) — a canary HeadObject, when configured

**Researcher-side checks** (only with `--account`):

- VPC Lattice / RAM Association
- Manifest_Bucket Cross-Account Read Grant
- Test_Lambda Discovery and Test_Lambda Invocation (when the probe is deployed)

All checks run to completion even if some fail (it does not abort on the first
failure). The command exits non-zero if **any** check failed.

**Example:**

```sh
$ data-releaser validate --account 534840902377
Provider-side validation results:
─────────────────────────────────
  [PASS] Credential Resolution (STS GetCallerIdentity)
  [PASS] OrcaVault Mart Reachability (Athena)
  [PASS] Authorization Service Reachability
  [PASS] Htsget_Rs Health (service-info)
  [PASS] Cross-Account S3 Access (Data Account)

Researcher-side validation results (account 534840902377):
─────────────────────────────────────────────────────
  [PASS] VPC Lattice / RAM Association
  [FAIL] Manifest_Bucket Cross-Account Read Grant
         the researcher's cross-account read grant is missing; the onboarding step that adds it to the Manifest_Bucket policy is incomplete
  [PASS] Test_Lambda Discovery
  [PASS] Test_Lambda Invocation

Summary: 7/8 checks passed
# exit status 1
```

Run `validate` after deploying the infrastructure and before onboarding a
researcher, to confirm the system is healthy independently of creating a real
production share.
