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

> `go build ./...` works. The CDK app's `infrastructure/node_modules/` carries
> `.go` files that are not part of this module, and `go.mod` excludes them with
> an `ignore` directive rather than leaving every build to be scoped by hand.

You can also run without building an artifact:

```sh
go run . <command> [flags]
```

## Authentication

Every command runs under a single set of credentials for the account this tool
runs in, resolved via the standard AWS credential chain (a named profile backed
by AWS SSO / IAM Identity Center, a `credential_process` provider, environment
variables, or an assumed role). You never authenticate into more than one
account: a researcher's account is named in a grant, never signed into.

Select the source with the `profile` config field or the `--profile` flag;
omit both to use the default chain. See
[`docs/configuration.md`](configuration.md) for details.

## Verbs and mechanisms

Data reaches a researcher two ways, and every invocation names which one:

- **`htsget`** — the researcher streams range requests through htsget-rs, and
  the authorization callout consults the share record on each one. The record
  *is* the enforcement point.
- **`s3`** — a copy of the dataset is held in this account and the researcher
  reads it directly. The cache bucket's policy and the policy of the key
  encrypting that bucket are the enforcement point, and AWS evaluates both on
  every request.

For the `s3` mechanism the ledger holds two kinds of record, and almost every
command below is about one or the other:

- A **copy** is one transfer of one dataset into this account. It owns the
  provenance of the bytes — which dataset, which bucket and key they staged in,
  which execution moved them, how much arrived — and nothing about who may read
  them. It moves `copying` → `staged` or `failed`, and later `purged` and
  `deleted`.
- A **grant** is one researcher account's access to one copy: their account, the
  VPC their reads must arrive from, and when the access ends. It is `granted`
  until it is `revoked`; `expired` is derived by comparing the recorded expiry
  against the clock and is never stored, which is what keeps an expired grant
  extendable.

Several grants stand against one copy, and the copy outlives all of them. That
is the whole reason the two are separate records: copying a dataset runs for
hours and is billed for it, while granting access to what it delivered is a
policy statement written in a second. One copy can be granted, withdrawn, and
granted again — to the same researcher or another — without anything being
transferred a second time.

The mechanism is named as part of the invocation and is **never** inferred from
which flags were given. The two take different inputs and grant access by
entirely different means, so choosing the wrong one is an access-control
mistake — and it is not one that fails at the time. It surfaces much later, as
an unexplained deny or as an unintended grant nobody is looking for. Guessing
from the flags would turn a typo into that.

That leaves two shapes:

- A verb that **changes access or the data behind it** — `stage`, `share`,
  `extend`, `revoke`, `purge`, `restore`, `delete` — refuses to act without a
  mechanism. Invoked bare it exits non-zero, names the mechanisms it knows, and
  lists the invocations that exist for that verb. It does not print help and
  exit `0`: for a verb meant to grant or stop access, that would report success
  for having done nothing, and a script cannot tell the difference.
- A verb that **only reads** — `list`, `validate` — takes the mechanism as an
  optional argument and covers both when it is omitted. Reporting on one
  mechanism only because the other was not named would hide access rather than
  mis-grant it.

| Verb | Mechanisms |
| --- | --- |
| `stage` | `s3` |
| `share` | `s3`, `htsget` |
| `extend` | `s3`, `htsget` |
| `revoke` | `s3`, `htsget` |
| `purge` | `s3` |
| `restore` | `s3` |
| `delete` | `s3` |
| `list` | optional — both when omitted |
| `validate` | optional — both when omitted |

`stage`, `purge`, `restore` and `delete` exist only for `s3`. They copy a dataset
into this account, destroy that copy, take that destruction back while its
recovery window is still running, and reclaim the storage the copy was
occupying — and there is nothing to copy, destroy, recover or reclaim where the
researcher reads through htsget-rs.

**Example — a mutating verb invoked bare:**

```sh
$ data-releaser share --libraries L2400001,L2400002 --account 777788889999
Error: data-releaser share requires a mechanism (s3 or htsget) and never infers one from
the other inputs given; run one of: data-releaser share s3, data-releaser share htsget
# exit status 1
```

Unknown flags are tolerated on the bare verb deliberately. `--libraries` is
perfectly valid one level down, so failing on the flag would point at the wrong
thing; the omission that matters is the mechanism, and that is what is
reported. The mechanism leaf still parses its own flags strictly.

**Example — a mechanism the verb has no leaf for:**

```sh
$ data-releaser purge htsget --dataset-uri s3://source-bucket/datasets/cohort-a/
Error: data-releaser purge does not support the htsget mechanism: purge implements s3
# exit status 1
```

Falling back to the verb's other mechanism is never the answer, so it is not
offered: the two grant different access.

## Global (persistent) flags

These flags are available on every subcommand:

| Flag | Description |
| --- | --- |
| `--config <path>` | Path to the config file. If omitted, the tool searches `./config.yaml` then `~/.config/data-releaser/config.yaml`. |
| `--profile <name>` | AWS profile override. Takes precedence over the `profile` field in the config file. |
| `-h`, `--help` | Help for the command. |

A config file must be resolvable for any command to run; if none is found, the
command fails before doing any work. *Which fields* within it are required
depends on the command, because the two mechanisms share a file and almost no
fields: each command declares the sections it reads, so one mechanism's missing
configuration never blocks the other mechanism's commands. The per-command
table is in [`docs/configuration.md`](configuration.md).

## Exit-code convention

The CLI follows a simple two-value convention:

- **`0`** — success. Note that some "empty" outcomes are still successes: `list`
  finding nothing prints an informative message and exits `0`, and so does
  `extend` against a grant that has no expiry to move.
- **non-zero** (`1`) — failure. Any error — invalid input, a missing mechanism,
  a failed preflight, an unreachable service, a resolution problem, a state
  conflict, an abandoned `purge` or `delete` confirmation, or one or more failed
  `validate` checks — is written to standard error prefixed with `Error:` and
  the process exits non-zero.

Where a command can only fail before it has changed anything, it says so: every
refusal above reports that nothing was written. Where a command changes more
than one thing — two policy documents, a ledger record, a researcher's manifest
— a failure part-way through is reported as exactly that, naming what took
effect, what did not, and what to run to bring them back together. Nothing is
silently unwound, because unwinding a grant that is already in force is itself
a change nobody asked for.

Results go to **standard output**; errors, warnings, and advisory notes go to
**standard error**.

---

## `stage` — copy a dataset into this account

Staging grants nobody anything. It puts a copy of the data where a grant can
later point at it, without the external source store ever being exposed.

What it records is a **copy**: one transfer of one dataset into this account.
Access is granted later, as grants of their own made against that copy, and
however many of those come and go the dataset is transferred once.

```
data-releaser stage s3 --dataset-uri <uri> [--objects-from <file>] [--dry-run] [--max-cost <usd>]
```

### `stage s3`

| Flag | Required | Description |
| --- | --- | --- |
| `--dataset-uri` | yes | URI of the dataset to copy, naming a folder in the source store (e.g. `s3://source-bucket/datasets/cohort-a/`). |
| `--objects-from` | no | Path to a file naming the dataset's objects, one key per line, instead of leaving enumeration to the copy service. |
| `--dry-run` | no | Check that every source object can be reached and that the destination can be written, and price the transfer, without copying any data. |
| `--max-cost` | no | Refuse the copy if the transfer is estimated to cost more than this many US dollars. Unlimited when omitted. |

**Dataset URIs.** Only the `s3` scheme is understood, and the scheme may be
omitted. Wildcards, empty path segments, and relative segments (`.`, `..`) are
rejected: the URI names a folder, and matching within it is the copy's
business.

The URI is also an identity. The bucket a dataset lands in is derived from the
name prefix, this account, the region, and the URI *exactly as written*, so a
URI spelled differently from the one the per-dataset deployment was given names
a different bucket — which is why a bucket is worth pinning by name in
configuration once it holds a copy someone is reading.

**Naming the objects yourself (`--objects-from`).** By default the copy service
is handed a wildcard over the dataset's folder and enumerates it with its own
credentials, which is what keeps the source store's credentials out of this tool
entirely. That default has a ceiling: the copy service expands one wildcard to a
fixed maximum number of objects and refuses the whole run above it, because the
result of heading that many objects has to fit in one step's payload. The run is
refused *entirely* rather than partly done, so nothing is staged and nothing is
granted — and the answer is to name the objects rather than to retry, since a
named object is headed on its own and carries no such limit.

The file is one object key per line. Blank lines and lines beginning with `#`
are ignored, so a list can carry a note about where it came from. Each key may
be written as a full `s3://` URI or as a key within the source bucket — both of
the forms a bucket listing produces. Every line is checked against the dataset it
claims to belong to and every bad line is reported at once: a key in another
bucket, or one not under the dataset's prefix, is refused rather than copied into
a cache bucket whose copy record does not describe it. Producing the list is the
operator's to do, because they hold the credentials to list the source store and
this tool deliberately does not:

```sh
aws s3api list-objects-v2 --bucket source-bucket --prefix datasets/cohort-a/ \
  --endpoint-url https://ceph.example.org \
  --query 'Contents[].Key' --output text | tr '\t' '\n' > cohort-a.objects
```

**What it does, in order:**

1. Reads and checks the flags. `--max-cost` cannot be combined with `--dry-run`
   (the priced run is what *produces* the estimate, so gating it on one would
   start the run and then refuse it), and a negative ceiling could never be met.
   Any `--objects-from` list is read and checked here too, before any
   configuration is loaded or any call is made: a list that does not describe
   this dataset costs nothing to catch now and a great deal to discover halfway
   through a transfer.
2. Parses the dataset URI, reducing it to one canonical form. Everything
   downstream — the copy record, the derived bucket name, the pinned-bucket
   lookup — uses that form, so a URI written with or without its scheme or its
   trailing slash names one dataset rather than several.
3. Resolves the cache bucket for the dataset: a name pinned in configuration if
   there is one, otherwise the derived name.
4. Reads the ledger for a copy that already accounts for the dataset. One that
   is `copying` or `staged` covers it and the command refuses — a second
   transfer would run for hours to deliver what is already there. A `failed`
   copy delivered nothing and does not stand in the way, and neither does a
   `purged` one, whose key is being destroyed and which therefore has to be
   transferred again to be readable at all. Whether anyone holds a grant on the
   covering copy makes no difference: a copy nobody holds is as much a reason
   not to transfer the dataset again as one two researchers are reading.
5. Confirms that the bucket exists and that the key encrypting it (found by the
   alias the per-dataset deployment gives it) resolves to an enabled key. Both
   are reported together, because an operator who has not deployed a dataset's
   resources is missing both. The key's ARN is what is recorded rather than the
   alias it was found by: a copy that cannot name its own key can never be
   purged, and an alias can be repointed.
6. With `--max-cost`, resolves the estimate the ceiling is checked against. The
   copy service takes no cost limit of its own and produces its estimate inside
   a run, so gating on cost is necessarily two invocations: if no estimate
   exists yet, this one starts a priced run that moves no data and refuses,
   naming the run and where its estimate will appear. Run the same command
   again once it has priced every object.
7. Starts the copy and records it in the `copying` state.

The copy runs for hours to days and its outcome arrives as an event, so the
command returns as soon as the copy has been started. Nothing can be granted
until the copy reports and moves to `staged` or `failed`.

**Example — start a copy:**

```sh
$ data-releaser stage s3 --dataset-uri s3://source-bucket/datasets/cohort-a/
Dataset staging started:
  Copy ID:      01JQ8XABCDEF0123456789ABCD
  Dataset:      s3://source-bucket/datasets/cohort-a/
  Bucket:       data-control-dataset-3f2a91c7b4e05d68
  Key:          arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-...
  Execution:    arn:aws:states:ap-southeast-2:999988887777:execution:steps-s3-copy:01JQ8X...
  Instructions: s3://steps-s3-copy-working/data-releaser/01JQ8XABCDEF0123456789ABCD/
  State:        copying

The copy runs for hours to days and reports its own outcome, which moves it to
staged or failed. Nothing is granted to anyone until it does.
```

With `--max-cost`, the estimate the ceiling was checked against is reported
alongside the rest as an `Estimate:` line, giving the dollar figure, the object
count, the byte count and when the pricing run observed them.

**Example — price the transfer without copying anything:**

```sh
$ data-releaser stage s3 --dataset-uri s3://source-bucket/datasets/cohort-a/ --dry-run
Priced run started; no data is being copied:
  Dataset:   s3://source-bucket/datasets/cohort-a/
  Bucket:    data-control-dataset-3f2a91c7b4e05d68
  Execution: arn:aws:states:ap-southeast-2:999988887777:execution:steps-s3-copy:01JQ8Y...
  Estimate:  s3://steps-s3-copy-working/data-releaser/... (written once every object has been priced)

Every source object is checked and the destination is written to, so the run
exercises both ends without staging anything.
```

A priced run and a real copy are reported differently on purpose: one has
staged a dataset and the other has committed to nothing.

**Example — the dataset is already accounted for (nothing was started):**

```sh
$ data-releaser stage s3 --dataset-uri s3://source-bucket/datasets/cohort-a/
Error: dataset "s3://source-bucket/datasets/cohort-a/" is already covered by copy
01JQ8XABCDEF0123456789ABCD, which is staged: the data is already in
data-control-dataset-3f2a91c7b4e05d68, so no copy was started. Grant access to it as it
stands, or purge the dataset first if it has to be copied again
# exit status 1
```

A transfer still in flight reports differently, naming the execution already
running and saying to wait for it rather than to grant.

**Example — the dataset was too large for one wildcard:**

A copy refused for that reason reports itself as `failed`, and the failure says
so rather than leaving the copy service's own error to be decoded:

```
the dataset holds more objects than one instruction covering the whole folder is
expanded to, so the copy service refused the run rather than copying part of it.
Nothing was staged and nothing was granted. Stage the dataset again naming its
objects, which are headed one by one and carry no such limit: ...
```

Stage it again with `--objects-from`. The counterpart failure — the folder
matched nothing at all — says that instead, and points at the dataset URI.

---

## `share` — grant a researcher account access

```
data-releaser share s3     --dataset-uri <uri> --account <id> --target-vpc-id <id> [--expiry <duration>]
data-releaser share htsget --libraries <ids> --account <id> [--target-vpc-arn <arn>] [--expiry <duration>]
```

**Duration format.** Both leaves accept a day suffix (`30d`, `7d`) or any
standard Go duration string (`24h`, `720h`, `48h`). A duration shorter than one
day is rejected on both, so that the same duration means the same thing
whichever mechanism is named and a transposed unit cannot grant minutes where
days were meant. Omitting `--expiry` gives an unlimited grant rather than a
distant one.

### `share s3`

Grant a researcher account read access to the copy of a dataset held in this
account, from one nominated VPC.

| Flag | Required | Description |
| --- | --- | --- |
| `--dataset-uri` | yes | URI of the dataset to grant access to. It must already have a `staged` copy here. |
| `--account` | yes | Researcher AWS account ID. Must be 12 digits: it goes into both policy documents as the principal being granted, so an alias or a role ARN names nobody. |
| `--target-vpc-id` | yes | The researcher's bare VPC identifier (`vpc-0123456789abcdef0`) that reads must arrive from. |
| `--expiry` | no | Expiry duration. Minimum 1 day. Unlimited if omitted. |

Access is the two documents and nothing else: the bucket policy and the key
policy, evaluated by AWS on every request. The grant takes effect as they are
written and needs nothing of ours running to sustain it — which is what fixes
the order below.

**Every invocation mints a new grant.** Sharing is not an edit of a record that
was already there. A researcher whose access was withdrawn and is then given
back holds a *second* grant, with an identifier of its own; the withdrawn one
keeps its record and its revocation timestamp, because a grant that ended is
part of the account of what happened. Nothing is copied for the second grant —
the objects never left the bucket.

**Several researchers may read one copy at once**, and one researcher may hold
access from two different VPCs, which is two grants. The CLI shares one at a
time. What is refused is the same account reading the same copy from the *same*
VPC twice: that is a duplicate of a grant already in place rather than a second
researcher, and the answer is to move that grant's expiry or to withdraw it.
Granting the same account access from a different VPC, or another account access
to the same copy, is a separate grant and is not refused.

**What it does, in order:**

1. Reads and checks every flag before the first call. None of these values
   fails visibly later: an account that is not an account ID names no
   principal, and a VPC in the wrong form matches no request, so both would
   produce a policy that is written successfully and admits nobody.
2. Resolves where the researcher's manifest is delivered, up front, so a
   manifest misconfiguration is found before any policy is in force.
3. Reads the dataset's copies and picks the newest `staged` one. Anything else
   is refused, naming the copy and the state it is in — still `copying`,
   `failed`, `purged`, or `deleted` — and no policy is written. Then reads every
   grant already standing against that copy, and refuses a duplicate of one of
   them.
4. Reads the state of the key. A key disabled or scheduled for deletion outside
   this tool leaves the ledger behind — the copy still reads as `staged` while
   the data it protects is already unreadable — and that makes the grant
   *undeployable* rather than merely undeployed, so it is caught before anything
   is written. The read commits to nothing either way.
5. Records the grant. This has to come first: the statements a grant contributes
   are named after the grant itself, which is what lets several of them sit in
   one pair of documents without colliding, so there is nothing to render until
   the ledger has minted the identifier.
6. Writes the two documents from the ledger — the key policy first, then the
   bucket policy, so the gate opens last. What is written is the **whole copy**
   rather than the one new grant: every grant standing against a copy
   contributes its own statements to the same two documents, and those documents
   are rendered whole and replace whatever is deployed, so rendering from the
   new grant alone would take every other researcher's access away as a side
   effect of giving one.
7. Republishes the researcher's dataset manifest, built from the ledger after it
   was written, so the document describes the access that was recorded rather
   than the access that was asked for.

Record before documents is the milder direction to fail in. A failure between
the two leaves a grant recorded and not deployed: the researcher cannot read,
because the documents a request is checked against say nothing about them, and
nobody holds access that no record mentions. The disagreement resolves towards
the record — reconciling the copy renders both documents from the ledger and
writes the grant out. Repeating the command does *not* finish the job, since it
is refused as a duplicate of that very grant, so the report says to reconcile or
to withdraw instead.

**Example — a 30-day grant:**

```sh
$ data-releaser share s3 --dataset-uri s3://source-bucket/datasets/cohort-a/ \
    --account 777788889999 --target-vpc-id vpc-0123456789abcdef0 --expiry 30d
Dataset released successfully:
  Grant ID:    01JQ8XABCDEF0123456789ABCD
  Copy ID:     01JQ8WBBBB0000000000000000
  Dataset:     s3://source-bucket/datasets/cohort-a/
  Account:     777788889999
  Target VPC:  vpc-0123456789abcdef0
  Bucket:      data-control-dataset-3f2a91c7b4e05d68
  Key:         arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-...
  Expiry:      2026-09-27T04:00:00Z
  Deployed:    2 grant(s) on this copy
  Applied:     key policy, bucket policy
  Manifest:    1 dataset(s) described to account 777788889999
Note: a grant can only be confirmed by reading an object from inside the target VPC;
a wrong VPC identifier, a passed expiry, and a key that cannot decrypt all present as
the same access denial.
```

`Deployed:` is the count of grants the documents that were just written carry.
It is there because the documents were rewritten whole: the count is what tells
an operator that granting this one left the others where they were.

The researcher's endpoint for S3, and which of their own principals may read,
are theirs to arrange. The grant names their account and bounds it to one VPC;
their IAM decides the rest.

**What one grant contributes to the two documents.** Three statements, and their
identifiers are the grant's own id with a fixed prefix, which is what lets
several grants sit in one pair of documents without colliding:

| Statement id | Document | What it does |
| --- | --- | --- |
| `AllowResearcher<grant-id>` | bucket policy | Allows `s3:GetObject` and `s3:ListBucket` to the researcher's account root, conditioned on `aws:SourceVpc` equalling the target VPC, plus a `DateLessThan` on `aws:CurrentTime` where the grant has an expiry. |
| `DenyResearcherControl<grant-id>` | bucket policy | Denies that one researcher everything under the copy's `_control/` prefix, so a grant over the bucket is not also a grant over how the copy ran or what it cost. |
| `AllowResearcherDecrypt<grant-id>` | key policy | Allows `kms:Decrypt` and `kms:DescribeKey` to the same principal, conditioned on `kms:ViaService` being S3 in this region, plus the same expiry condition. |

Two things about those statements are worth knowing when reading a deployed
document. The principal is the researcher's **account root**, which delegates to
their own IAM the question of which of their roles may read — the VPC condition
is what actually bounds the grant, so widening the principal does not widen where
reads can come from. And there is deliberately **no VPC condition on the key**:
when S3 decrypts on a requester's behalf the call to the key comes from S3's own
infrastructure rather than from the requester's VPC, so a VPC condition there
can deny a request that did originate in the permitted VPC. The VPC bound is the
bucket policy's to enforce, and it is enforced before any decryption is asked
for.

An unlimited grant carries **no time condition at all** rather than a distant
date. A date would be a lie about what was agreed, would eventually arrive, and
would make an unlimited grant indistinguishable from a long one when the deployed
document is read back.

Alongside these, every render also writes the statements that are there whoever
is granted: `DenyInsecureTransport` on the bucket, and
`AllowAccountKeyAdministration`, `AllowCopyWriterToEncrypt` and — where a
custodian is configured — `AllowCustodianToWithdraw` on the key. They are
re-rendered every time because a rendered document replaces the deployed one
whole.

**Example — a duplicate of a grant already in place:**

```sh
$ data-releaser share s3 --dataset-uri s3://source-bucket/datasets/cohort-a/ \
    --account 777788889999 --target-vpc-id vpc-0123456789abcdef0 --expiry 30d
Error: account 777788889999 already reads dataset "s3://source-bucket/datasets/cohort-a/"
from vpc-0123456789abcdef0 under grant 01JQ8XABCDEF0123456789ABCD, which runs until
2026-09-27T04:00:00Z. A second grant would deploy statements saying the same thing and
leave two records where one describes the access, so nothing was written. Move that
grant's expiry to change how long it runs, or withdraw it if the access should end.
Granting the same account access from a different VPC, or another account access to the
same copy, is a separate grant and is not refused
# exit status 1
```

### `share htsget`

Resolve library IDs to S3 locations, create a share record, and deliver a
manifest to the researcher account.

| Flag | Required | Description |
| --- | --- | --- |
| `--libraries` | yes | Comma-separated list of library IDs to share (e.g. `L2400001,L2400002`). |
| `--account` | yes | Target researcher AWS account ID. |
| `--target-vpc-arn` | no | Full EC2 ARN of the researcher VPC associated with the htsget VPC Lattice service network. Overrides `researchers.<account>.targetVpcArn` from the config file. |
| `--expiry` | no | Expiry duration. Minimum 1 day. Unlimited if omitted. |

**What it does, in order:**

1. Validates inputs (rejects an empty library list, a missing account, or an
   expiry below 1 day) before any network call.
2. Resolves the researcher's VPC ARN — from the flag if given, otherwise from
   the `researchers` section — and validates it, including that it belongs to
   the account being shared with. There is deliberately no fallback and no
   derivation: the authorization callout looks this value up by exact match, so
   a guessed ARN does not degrade gracefully. It writes a share that can never
   authorize anything, and the failure surfaces later as an unexplained deny.
3. Preflight: verifies the researcher account has an active VPC Lattice / RAM
   association; refuses if not.
4. Preflight: verifies the manifest bucket exists and is writable by this
   account's identity, before any share record is created. That removes the
   window where a share is persisted and only then fails on the manifest upload.
5. Resolves the libraries with a single Athena query against `mart.bam`.
6. Validates resolution: aborts if any library has no shareable reads or reports
   an archived storage class (reporting all affected IDs at once).
7. Performs an idempotency check and creates the share record (or reuses an
   existing identical active share).
8. Generates and uploads the manifest for the researcher. A failure here is
   reported as an error naming the share that *was* created, so the operator
   knows to retry the upload rather than the share.

**Example — create a 30-day share:**

```sh
$ data-releaser share htsget --libraries L2400001,L2400002 --account 777788889999 --expiry 30d
Share created successfully:
  Share ID:  01JQ8XABCDEF0123456789ABCD
  Account:   777788889999
  Libraries: 2 (L2400001, L2400002)
  Expiry:    2026-09-27T04:00:00Z
```

**Example — unlimited share (no `--expiry`):**

```sh
$ data-releaser share htsget --libraries L2400001 --account 777788889999
Share created successfully:
  Share ID:  01JQ8XABCDEF0123456789ABCD
  Account:   777788889999
  Libraries: 1 (L2400001)
  Expiry:    unlimited
```

**Example — idempotent re-run (same libraries + account as an active share):**

```sh
$ data-releaser share htsget --libraries L2400001,L2400002 --account 777788889999 --expiry 30d
Existing share reused (idempotent match):
  Share ID:  01JQ8XABCDEF0123456789ABCD
  Account:   777788889999
  Libraries: 2 (L2400001, L2400002)
  Expiry:    2026-09-27T04:00:00Z
```

The existing share's identifier and expiry are returned unchanged; no duplicate
is created, and a differing `--expiry` does not modify the existing share. A
share whose library set only *partially* overlaps an existing share creates a
new share instead.

**Example — resolution problem (aborts, no partial share):**

```sh
$ data-releaser share htsget --libraries L2400001,L9999999 --account 777788889999
Error: cannot share libraries:
  library ID L9999999 may not exist, may not be aligned yet, or may have reads that are archived
# exit status 1
```

An archived read reports similarly, naming the offending storage class, e.g.
`library ID L2400003 has archived storage class DeepArchive and cannot be
streamed by htsget-rs`.

### The two VPC flags are not interchangeable

Both leaves take the researcher's VPC, and they take it in two different forms.
The names are near-homographs and the consequence of confusing them is
invisible, so it is worth stating plainly:

| Flag | Form | Why that form |
| --- | --- | --- |
| `share htsget --target-vpc-arn` | Full EC2 VPC ARN, `arn:aws:ec2:<region>:<account-id>:vpc/vpc-<id>` | It is what VPC Lattice reports as the source of a request, and what the authorization callout matches on. |
| `share s3 --target-vpc-id` | Bare identifier, `vpc-<id>` | The bucket policy condition compares this value **literally** against the VPC a read arrived from. |

Passing an ARN where the bare identifier belongs writes a policy happily and
denies every read; passing a bare identifier where the ARN belongs writes a
share record the callout can never match. `share s3` rejects anything that
looks like an ARN for exactly this reason, and points at the leaf that wants
one — but the reverse confusion inside a config file is not something either
leaf can catch, so check the form when a researcher reports a denial nobody can
account for.

---

## `extend` — move the expiry of an existing grant

Access continues without the data being shared or copied again.

```
data-releaser extend s3     <grant-id> --duration <duration>
data-releaser extend htsget <share-id> --duration <duration>
```

Both leaves take the record's identifier as their only positional argument and
the same `--duration` syntax, and on both the new expiry is computed as *now +
duration* — it is not added to the existing expiry. The minimum is 1 day. The
identifier to pass is the `ID` column of `list`.

### `extend s3`

| Argument / flag | Required | Description |
| --- | --- | --- |
| `<grant-id>` | yes | The grant whose expiry is being moved (positional). |
| `--duration` | yes | New duration from now. Minimum 1 day. |

**The operand is a grant, not the copy it reads from.** Several researchers may
hold grants on one copy, and moving one researcher's expiry leaves the others
exactly as they were — the two documents carry every grant's statements and are
rendered whole from all of them, so the others are written back precisely as the
ledger holds them.

The expiry is a condition inside those two deployed documents, not a job that
runs somewhere, so moving it is two writes: the recorded expiry and the time
condition.

**What it does, in order:**

1. Reads and checks the operand and the duration.
2. Reads the grant, for the report — so the output can name the expiry being
   replaced, which is the difference between an extension that widened access
   and one that narrowed it.
3. Reads the copy the grant reads from, before anything is written. A copy that
   is `purged` can carry no expiry worth moving: no date reaches data on its way
   out, and recording one would report success for access nothing will honour.
4. Writes the record first. The ledger mints the new expiry from its own clock
   as it writes, and the documents are then rendered from the record that came
   back. Rendering them first would commit to a moment the ledger had not
   agreed to, and since the two policy writes take long enough for the ledger's
   clock to land in a later second — and the condition is compared to whole
   seconds — every extension would leave a mismatch that reconciliation reports
   as drift.
5. Applies both documents, rendered from every grant the ledger now holds
   against the copy.
6. Republishes the researcher's dataset manifest, which carries the expiry.

`share s3` records first as well, but for a reason of its own: a grant's
statements are named after the grant, so there is nothing to render until the
ledger has minted it. The two commands arrive at the same order from different
directions.

Writing the record first also fails closed in the direction this command is
normally used: while the two are apart the record claims access the documents
have not granted yet. A duration *shorter* than the time remaining moves the
expiry inward, and there the same failure leaves real access outliving the
record — so a failure between the halves says which of the two happened, since
one can be retried at leisure and the other cannot.

A grant whose expiry has already passed is extendable: its granting statements
are still deployed and the objects are still in place, so moving the expiry
forward restores access without another copy. When that happens the command says
so on standard error, because restored access is no more observable from here
than granted access was.

**Example — move an expiry out by 60 days:**

```sh
$ data-releaser extend s3 01JQ8XABCDEF0123456789ABCD --duration 60d
Grant expiry moved:
  Grant ID:    01JQ8XABCDEF0123456789ABCD
  Copy ID:     01JQ8WBBBB0000000000000000
  Dataset:     s3://source-bucket/datasets/cohort-a/
  Account:     777788889999
  Target VPC:  vpc-0123456789abcdef0
  State:       granted
  Was:         2026-09-27T04:00:00Z
  Expiry:      2026-10-27T04:00:00Z
  Deployed:    2 grant(s) on this copy
  Applied:     key policy, bucket policy
  Manifest:    1 dataset(s) described to account 777788889999
```

**Example — a grant made without an expiry:**

```sh
$ data-releaser extend s3 01JQ8XABCDEF0123456789ABCD --duration 60d
No expiry applies; nothing was changed:
  Grant ID:    01JQ8XABCDEF0123456789ABCD
  Copy ID:     01JQ8WBBBB0000000000000000
  Dataset:     s3://source-bucket/datasets/cohort-a/
  Account:     777788889999
  Target VPC:  vpc-0123456789abcdef0
  Expiry:      unlimited

The grant was made without an expiry, so its deployed policies carry no time
condition and access does not lapse. There is nothing for a duration to move;
stopping this access is a withdrawal rather than an expiry.
```

This exits `0`: the state the operator was reaching for already holds, and
nothing is written for it. A grant carrying no time condition and one carrying a
distant date are different grants, and quietly turning the first into the second
would narrow an agreement nobody asked to narrow.

**Error cases** (each exits non-zero, leaving both documents and the record
exactly as they were):

```sh
$ data-releaser extend s3 UNKNOWNID --duration 30d
Error: grant "UNKNOWNID" not found. Nothing was changed; run 'data-releaser list s3'
to see the grants and the ids they are extended by

$ data-releaser extend s3 01JQ8XABCDEF0123456789ABCD --duration 30d
Error: grant 01JQ8XABCDEF0123456789ABCD is revoked: the granting statements are already
out of the deployed policies, so there is no time condition left to move and moving one
would not give access back. The objects and the key are untouched, and nothing was
written. Granting the dataset again is what restores access, as a new grant with a
record of its own
```

A grant whose copy is being destroyed is refused too, for a different reason:
deletion of the key encrypting that copy is scheduled, so no expiry could reach
the data. Take the purge back with `restore s3` if the recovery window is still
running, or stage the dataset again to make a fresh copy grantable.

### `extend htsget`

| Argument / flag | Required | Description |
| --- | --- | --- |
| `<share-id>` | yes | The share whose expiry is being moved (positional). |
| `--duration` | yes | New duration from now. Minimum 1 day. |

Updates the expiry of an active share record and regenerates the researcher's
manifest. There are no policy documents here — the callout reads the record on
every request — so the record is the whole of the change.

**Example — extend by 60 days:**

```sh
$ data-releaser extend htsget 01JQ8XABCDEF0123456789ABCD --duration 60d
Share extended successfully:
  Share ID:   01JQ8XABCDEF0123456789ABCD
  Account:    777788889999
  Libraries:  2 (L2400001, L2400002)
  New Expiry: 2026-10-27T02:30:00Z
```

**Example — extending an unlimited share:**

```sh
$ data-releaser extend htsget 01JQ8XABCDEF0123456789ABCD --duration 60d
Share 01JQ8XABCDEF0123456789ABCD already has unlimited duration; no extension needed.
```

An unlimited share already grants access indefinitely, so no extension is
applied and the command exits `0`.

**Error cases** (each exits non-zero, leaving state unchanged):

```sh
$ data-releaser extend htsget UNKNOWNID --duration 30d
Error: share "UNKNOWNID" not found

$ data-releaser extend htsget 01JQ8XABCDEF0123456789ABCD --duration 30d
Error: share "01JQ8XABCDEF0123456789ABCD" has been revoked and cannot be extended

$ data-releaser extend htsget 01JQ8XABCDEF0123456789ABCD --duration 30d
Error: share "01JQ8XABCDEF0123456789ABCD" has already expired and cannot be extended
```

Unlike an s3 grant, an expired share is *not* extendable: there is no
deployed grant left standing that moving a date would revive. Create a new
share instead.

The extension itself is durable before the manifest is regenerated, so a
manifest failure is reported as an error naming the share that was extended —
the researcher's manifest now understates their access, and the fix is to retry.

---

## `revoke` — stop a researcher account's access

```
data-releaser revoke s3     <grant-id>
data-releaser revoke htsget <share-id>
```

### `revoke s3`

| Argument / flag | Required | Description |
| --- | --- | --- |
| `<grant-id>` | yes | The grant to withdraw (positional). |

**The operand is a grant, not the copy it reads from.** Several researchers may
hold grants on one copy, and withdrawing one leaves the others deployed exactly
as they were. One withdrawal, one researcher.

Nothing is destroyed. The objects remain in the bucket, the key remains able to
decrypt them, and the dataset can be granted again — to the same account or
another — without being copied a second time. That later grant is a **new**
grant with a record of its own: this one's withdrawal stays on the record.
Destroying the copy is `purge`, which has its own confirmation because it is the
one that cannot be undone.

Withdrawing is not a separate operation on the two documents. They are rendered
from every grant standing against the copy, and a grant that has been withdrawn
contributes nothing, so writing them again with this grant taken out is the
whole of it. Withdrawing the last grant on a copy leaves the baseline pair — the
same pair a copy nobody was ever granted has.

**What it does, in order:**

1. Reads the grant and refuses unless its stored state is `granted`. A grant
   whose expiry has passed still counts: its granting statements are still
   deployed — expiry is a condition inside them rather than their removal — so
   taking them out is a real change.
2. Reads the copy and every other grant against it, and applies both documents
   with this grant described as withdrawn and every other one exactly as the
   ledger holds it. The bucket policy goes **first** — if only one write lands
   it is the one that closes the gate, and reads and listings have already
   stopped.
3. Records the withdrawal.
4. Republishes the researcher's dataset manifest, so their own description
   stops advertising access that has ended. An account whose last grant this was
   has its document removed rather than left behind.

Policies before the record, which is the *opposite* of the order a grant is made
in, and for a reason a grant does not have. Granting records first because a
grant's statements are named after the grant, so nothing can be rendered until
the record exists. Withdrawing has a record already, so it is free to choose,
and what it has to choose between is which half of a half-finished withdrawal to
leave behind. The direction to avoid is a record saying access has stopped while
it has not: that reports the custodian's intent as achieved when nothing was
withdrawn, and nobody investigates a listing that shows no access while the
researcher keeps reading. Applying first fails the other way — the reads have
stopped and only the bookkeeping is behind — which is still reported loudly,
because a grant that reads as deployed is what reconciliation would write back
out into the policies. Re-running the same withdrawal is the fix.

**Example — withdraw access:**

```sh
$ data-releaser revoke s3 01JQ8XABCDEF0123456789ABCD
Dataset access withdrawn:
  Grant ID:    01JQ8XABCDEF0123456789ABCD
  Copy ID:     01JQ8WBBBB0000000000000000
  Dataset:     s3://source-bucket/datasets/cohort-a/
  Account:     777788889999
  Target VPC:  vpc-0123456789abcdef0
  Bucket:      data-control-dataset-3f2a91c7b4e05d68
  Key:         arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-...
  Was until:   2026-10-27T04:00:00Z
  Withdrawn:   2026-08-28T02:30:00Z
  Deployed:    1 grant(s) on this copy
  Applied:     bucket policy, key policy
  Manifest:    removed; account 777788889999 holds no dataset access to describe

The objects in data-control-dataset-3f2a91c7b4e05d68 and the key encrypting them are
untouched: nothing was deleted, and the copy is still readable by this account. The
dataset can be granted again, to this researcher or another, without being copied a
second time -- as a new grant, since this one's withdrawal stays on the record.
Destroying the copy is a separate operation.

1 other grant(s) on copy 01JQ8WBBBB0000000000000000 were written back exactly as they
were, so the researchers holding them are reading as before.
```

That last paragraph appears only where the copy still carries grants. It is
there because the documents were rewritten whole, and it says what went back
into them.

A grant that was already withdrawn is refused with the state it is in, naming
the date its statements came out, and no policy is written.

### `revoke htsget`

| Argument / flag | Required | Description |
| --- | --- | --- |
| `<share-id>` | yes | The share to revoke (positional). |

Marks the share record revoked, immediately and durably, and regenerates the
researcher's manifest (deleting it if no active shares remain for the account).
Because authorization decisions are cached by htsget-rs, a revocation takes full
effect only once the relevant cached decisions expire — bounded by the
configured Cache-TTL ceiling. There is no way to shorten that window for an
individual revocation: the cache holds no externally invalidatable entries, so
the ceiling is the propagation bound in every case.

This leaf reads only the ledger, deliberately. Stopping access must not be
blocked by configuration belonging to the granting path, so the manifest is
republished afterwards on a best-effort basis and a failure there is a warning
rather than a refusal — the revocation is already durable, and re-running would
only report a conflict.

**Example — revoke a share:**

```sh
$ data-releaser revoke htsget 01JQ8XABCDEF0123456789ABCD
Share revoked successfully:
  Share ID:    01JQ8XABCDEF0123456789ABCD
  Revoked at:  2026-08-28T02:30:00Z
Note: propagation is bounded by the Cache-TTL ceiling.
```

**Error cases** (each exits non-zero, leaving state unchanged):

```sh
$ data-releaser revoke htsget UNKNOWNID
Error: share "UNKNOWNID" not found

$ data-releaser revoke htsget 01JQ8XABCDEF0123456789ABCD
Error: share "01JQ8XABCDEF0123456789ABCD" is already revoked

$ data-releaser revoke htsget 01JQ8XABCDEF0123456789ABCD
Error: share "01JQ8XABCDEF0123456789ABCD" has already expired
```

> Library sets are immutable — there is no in-place edit of which libraries a
> share grants. To change a researcher's libraries, revoke the affected share
> and create a new one (revoke-and-recreate).

---

## `purge` — destroy this account's copy of a dataset

```
data-releaser purge s3 --dataset-uri <uri> [--pending-window-days <n>] [--yes]
```

This is the only irreversible thing the tool does. The copy is destroyed by
scheduling deletion of the key it is encrypted under, so once the recovery
window elapses the objects in the cache bucket are ciphertext nobody can
read — not the researcher, not this account, not us. The authoritative copy in
the external source store is untouched and is never read or written here, which
is why a purged dataset can be copied in again from scratch. Anything a
researcher already downloaded is beyond the reach of this or any other command.

### `purge s3`

| Flag | Required | Description |
| --- | --- | --- |
| `--dataset-uri` | yes | URI of the dataset whose copy in this account is to be destroyed. |
| `--pending-window-days` | no | Days before deletion of the key becomes irreversible. Between 7 and 30; defaults to 30. |
| `--yes` | no | Destroy the copy without asking. |

**It is keyed on the dataset, not on a copy or a grant.** One key encrypts one
dataset's bucket, so the unit destroyed is the dataset: every copy of that
dataset and every grant made against those copies dies together. All of them are
therefore listed before anything is asked, including the ones the operator was
not thinking about — those are exactly the ones worth showing.

**The window is the only way back.** A scheduled deletion can be cancelled
until its date passes, and cancelling it makes the key able to decrypt again.
KMS accepts 7 to 30 days and no other value, and that period is the only one in
which a purge decided in error can still be taken back — which is why the
longest window is the default, and why the bound is enforced
here rather than left to KMS: the confirmation quotes the number the schedule
will actually carry. Where a key is *already* counting down, the date reported
is the one it is already going by, which may be much sooner.

**Confirmation is the dataset URI, not "yes".** For the one operation that
cannot be undone, naming what is being destroyed costs a copy and paste and
will not be given by accident to a prompt that appeared where it was not
expected. Anything else — including an invocation with nothing on its input at
all — abandons the purge, which exits non-zero and changes nothing, so a script
can tell a copy that was destroyed from one that was left alone. A script that
meant to destroy something says so with `--yes`.

**What it does, in order:**

1. Reads and checks the flags, including the window bounds, and reduces the
   dataset URI to the canonical form the records are keyed on — which is also
   the form the confirmation asks to be typed back.
2. Reads every copy of the dataset and every grant made against them. A dataset
   that has never been copied here is refused, with the reminder that a URI
   spelled differently names a different dataset.
3. Inspects every key the dataset's copies name, reporting any it cannot read
   together and scheduling nothing. A key already on its way out is recognised
   as such rather than scheduled a second time — which is also what lets a
   purge begun outside this tool be finished by it.
4. If every key is already scheduled, every grant already withdrawn, and every
   copy already recorded as purged, reports that and exits `0` without
   scheduling anything.
5. Prints the plan: every copy and every grant, the buckets and keys, the grants
   still deployed, and the date after which it is irreversible.
6. Asks for confirmation, unless `--yes`.
7. Takes the granting statements out of the bucket and key policies for every
   grant still deployed, and records each withdrawal. This comes first precisely
   *because* scheduling can be cancelled: a cancelled deletion restores the key,
   and a grant left in the policies would come back to life with it. One pair of
   writes per copy takes every one of its grants out at once, so there is no
   intermediate document that still admits half of them.
8. Schedules deletion of each key. This is the point of no return, so the date
   reported comes back from KMS rather than being computed from the window that
   was asked for.
9. Records every copy as `purged`. This runs last because it is the only
   reversible part; a record that could not be written is a reporting failure
   rather than a discrepancy in who can read what, and re-running records it
   without scheduling anything further.
10. Republishes the manifest of every researcher account that had access, each
    attempted whatever the ones before it did.

**Example — a purge, confirmed interactively:**

```sh
$ data-releaser purge s3 --dataset-uri s3://source-bucket/datasets/cohort-a/
Purging dataset s3://source-bucket/datasets/cohort-a/ destroys the copy of it held in this account.

Destroyed with it (1 copy record(s), 2 grant(s)):
MECHANISM  ID                          ACCOUNT       SHARED                                 TARGET VPC             CREATED               EXPIRY                STATE
s3         01JQ8XABCDEF0123456789ABCD  777788889999  s3://source-bucket/datasets/cohort-a/  vpc-0123456789abcdef0  2026-08-01T04:00:00Z  2026-10-27T04:00:00Z  granted
s3         01JQ8WBBBB0000000000000000  -             s3://source-bucket/datasets/cohort-a/  -                      2026-07-20T01:00:00Z  -                     staged
s3         01JQ7WZZZZ0000000000000000  111111111111  s3://source-bucket/datasets/cohort-a/  vpc-0aaaaaaaaaaaaaaa1  2026-05-01T00:00:00Z  2026-06-01T00:00:00Z  revoked

What is destroyed:
  Bucket:  data-control-dataset-3f2a91c7b4e05d68
  Key:     arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-... (Enabled)

Access withdrawn first (1 grant(s) still deployed):
  01JQ8XABCDEF0123456789ABCD to account 777788889999 from vpc-0123456789abcdef0, expiry 2026-10-27T04:00:00Z

Deleting the key is how the copy is destroyed: every object in the bucket above
is encrypted under it and nothing can read them once it is gone -- not the
researcher, not this account, not us. The deletion can be cancelled until
2026-09-27T02:30:00Z, after which it is irreversible and the copy is lost for good.

The authoritative copy in the external source store is untouched, so the dataset
can be copied in again from scratch. Anything already downloaded by a researcher
is beyond reach of this or any other command.

To destroy this copy, type the dataset URI (s3://source-bucket/datasets/cohort-a/). Anything else abandons the purge: s3://source-bucket/datasets/cohort-a/

Access withdrawn: grant 01JQ8XABCDEF0123456789ABCD no longer gives account 777788889999 anything (bucket policy, key policy)
Key scheduled for deletion: arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-..., irreversible from 2026-09-27T02:30:00Z

Dataset purged:
  Dataset:  s3://source-bucket/datasets/cohort-a/
  Key:      arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-...
  Bucket:   data-control-dataset-3f2a91c7b4e05d68
  Copy:     01JQ8WBBBB0000000000000000 now purged, irreversible from 2026-09-27T02:30:00Z

Access has already stopped: a key pending deletion decrypts nothing. Until the date
above the deletion can still be cancelled, which would leave the objects readable by
this account but granted to nobody.
```

The plan's table shows copies and grants together because neither answers on its
own: a copy says what was transferred and where it staged, and a grant says who
has been reading it. A copy fills the `ACCOUNT`, `TARGET VPC` and `EXPIRY`
columns with `-` and means it — those are a grant's business and never a copy's.

**Example — the confirmation was abandoned:**

```sh
$ data-releaser purge s3 --dataset-uri s3://source-bucket/datasets/cohort-a/
...
To destroy this copy, type the dataset URI (s3://source-bucket/datasets/cohort-a/). Anything else abandons the purge: no

Error: purge of dataset "s3://source-bucket/datasets/cohort-a/" was abandoned: no key
deletion was scheduled, no policy was written, and its 1 copy record(s) and 2 grant(s)
are exactly as they were
# exit status 1
```

**Example — the dataset is already purged:**

```sh
$ data-releaser purge s3 --dataset-uri s3://source-bucket/datasets/cohort-a/
Dataset s3://source-bucket/datasets/cohort-a/ is already purged; nothing was scheduled.
...
```

This exits `0`. Nothing is scheduled a second time and the dates reported are
the ones already in force.

---

## `restore` — take a purge back while its window is still open

```
data-releaser restore s3 --dataset-uri <uri>
```

A purge destroys this account's copy by scheduling deletion of the key it is
encrypted under, and KMS keeps that schedule cancellable for the whole of its
pending window — 7 to 30 days — precisely so a decision made in error can be
undone. This is the command that does the undoing.

The copy is already in the bucket and **nothing is transferred a second time**.
Without this command the tool could only schedule, never unschedule: an operator
would have to drop to raw AWS, and even then the ledger would still read
`purged`, so a grant would be refused and the only way forward would be copying
in again a dataset that had been sitting in the bucket, intact, the whole time.

### `restore s3`

| Flag | Required | Description |
| --- | --- | --- |
| `--dataset-uri` | yes | URI of the dataset whose purge is to be taken back. |

There is no confirmation flag and no prompt: a restore takes nothing away.

**It is keyed on the dataset, not on a copy**, for the same reason a purge is:
one key encrypts one dataset's bucket, so cancelling its deletion recovers every
copy of that dataset at once.

**Nobody has access afterwards.** This is the thing most likely to be assumed
the other way round. A purge withdraws every grant *before* it schedules the
key — which is exactly what makes cancelling safe — and those grants stay
withdrawn. Cancelling gives none of them back. Share the dataset again to grant
access, which costs nothing beyond the policy write.

**Cancelling is not the whole of it.** A cancelled deletion leaves the key
`Disabled`, not `Enabled`, and a disabled key decrypts nothing — which presents
to a researcher exactly as the purge did. So the key is enabled afterwards and
then read back, and a key that does not report itself enabled fails the run
loudly rather than leaving a restore that looks done and works for nobody.

**What it does, in order:**

1. Reads every copy of the dataset. One that has never been copied here is
   refused, and so is a dataset whose objects have been `deleted` — deleting
   them has no window and no way back, and cancelling the key deletion now would
   recover the ability to decrypt objects that are not there.
2. Refuses a dataset with no `purged` copy, which is what an already-restored
   one and a never-purged one both look like. Either way its copy is readable
   and can be shared as it stands.
3. Reads every grant, defensively: a grant still deployed here would have its
   access handed back the moment the key can decrypt again, without anybody
   granting it, so that is refused too.
4. Reads the state of every key the copies name, reporting all of them together.
   A key that is gone is the window having elapsed — KMS deletes the key when
   the schedule matures, and with it the only thing that could ever decrypt the
   objects — and that is reported differently from a key present in a state this
   cannot act on. Nothing has been written by this point, so a refusal here
   leaves the purge exactly as it was.
5. Cancels the scheduled deletion of each key, enables it, and reads it back.
6. Records every purged copy as `staged` again. This runs last because it is the
   only reversible part: a failure between the two leaves copies reading
   `purged` against a key that works, which grants nobody anything and is put
   right by running the command again.

**Example:**

```sh
$ data-releaser restore s3 --dataset-uri s3://source-bucket/datasets/cohort-a/
Restoring dataset s3://source-bucket/datasets/cohort-a/ takes back the purge of the copy held in this account.

Staged again (1 of 1 copy record(s), 2 grant(s) left withdrawn):
MECHANISM  ID                          ACCOUNT       SHARED                                 TARGET VPC             CREATED               EXPIRY                STATE
s3         01JQ8XABCDEF0123456789ABCD  777788889999  s3://source-bucket/datasets/cohort-a/  vpc-0123456789abcdef0  2026-08-01T04:00:00Z  2026-10-27T04:00:00Z  revoked
s3         01JQ8WBBBB0000000000000000  -             s3://source-bucket/datasets/cohort-a/  -                      2026-07-20T01:00:00Z  -                     purged
s3         01JQ7WZZZZ0000000000000000  111111111111  s3://source-bucket/datasets/cohort-a/  vpc-0aaaaaaaaaaaaaaa1  2026-05-01T00:00:00Z  2026-06-01T00:00:00Z  revoked

What is recovered:
  Bucket:  data-control-dataset-3f2a91c7b4e05d68
  Key:     arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-... (PendingDeletion)
           its deletion becomes irreversible at 2026-09-27T02:30:00Z

Key deletion cancelled: arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-...
Key enabled: arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-... (Enabled)

Dataset restored:
  Dataset:  s3://source-bucket/datasets/cohort-a/
  Key:      arn:aws:kms:ap-southeast-2:999988887777:key/2b1c9e0a-...
  Bucket:   data-control-dataset-3f2a91c7b4e05d68
  Copy:     01JQ8WBBBB0000000000000000 now staged

Nobody has access yet. A purge withdraws every grant before it schedules the
key -- which is what makes cancelling the deletion safe -- and those 2 grant(s)
stay withdrawn: cancelling gives none of them back. Share the dataset again to
grant access, which costs nothing beyond the policy write, since the objects
never left the bucket and are not transferred a second time.
```

The withdrawn grants are listed for what they do *not* say. An operator taking a
purge back is usually thinking about a researcher who lost access, and the
answer — that every one of those grants is still withdrawn — is easier to take
from a listing that names them than from a sentence.

A key that is already usable is reported as such and nothing is cancelled for
it, so a run interrupted between the cancel and the enable is finished by
running the command again.

---

## `delete` — delete a purged dataset's objects

```
data-releaser delete s3 --dataset-uri <uri> [--yes]
```

This reclaims storage and nothing else. Access to the dataset ended when it was
purged: a key pending deletion decrypts nothing from the moment it is
scheduled, so what is deleted here is ciphertext nobody can read, and the
bucket's lifecycle rule would expire it on its own regardless — a year after it
was written, unless the deployment set another `dataRetentionDays`. **To stop
somebody reading, purge the dataset — this is not a second way of doing that.**
What it buys is the storage now rather than whenever that rule gets to it.

It is nonetheless the one thing here with no way back at all. A scheduled key
deletion can be cancelled for as long as its window runs — that is what
[`restore s3`](#restore--take-a-purge-back-while-its-window-is-still-open) does,
and it restores a purged copy completely, because the objects are still there
and the key can read them again. Deleting the objects takes that path away too:
there is no window, nothing to cancel, and they are gone the moment the call
returns.

### `delete s3`

| Flag | Required | Description |
| --- | --- | --- |
| `--dataset-uri` | yes | URI of the dataset whose copied objects are to be deleted. |
| `--yes` | no | Delete the objects without asking. |

**It is keyed on the dataset, not on a copy.** The bucket is deployed per
dataset and every copy of that dataset stages into it, so the objects belong to
all of them. Every copy must therefore be `purged` or `failed`; a single one
still `staged` or `copying` refuses the whole dataset and is named in the
refusal, because emptying the bucket would take the data out from under whoever
is reading through it. A `failed` copy needs no purge first: it left part of a
dataset behind that was never granted to anybody and never will be.

**A grant still deployed refuses it too.** A purge withdraws every grant of the
dataset before it schedules the key, so a grant still in the deployed documents
means a purge did not finish — which is exactly when deleting the objects
underneath it would be worst.

**Confirmation is the dataset URI, not "yes"**, on the same reasoning as
`purge`, and with even less of a way back. Anything else abandons the deletion,
which exits non-zero and deletes nothing.

**The bucket itself stays.** It is deployed by the per-dataset stack, which
retains it on purpose, so removing it out of band would leave that stack
describing a bucket that is not there. Emptying it is the right granularity,
and the dataset can be staged into the same place again afterwards.

**What it does, in order:**

1. Reads every copy of the dataset. A dataset never copied here is refused.
2. Refuses if any copy could still be read, naming each one and its state, and
   refuses if any grant of the dataset is still deployed. Neither refusal
   reads the bucket at all.
3. Lists the bucket to exhaustion and reports how many objects are about to go
   and what they weigh, together with every copy the deletion will be recorded
   against.
4. Asks for confirmation, unless `--yes`.
5. Deletes every object, in batches of the 1000 keys the API accepts, the
   copy's own `_control/` artefacts included — they describe a transfer whose
   objects are going, and nothing reads them afterwards. Per-object failures
   reported in a batch's response body stop the run, because a request that
   refuses half its keys still returns successfully.
6. Records every copy as `deleted`.

The whole bucket is listed rather than the dataset's own prefix within it: the
bucket holds one dataset and the artefacts of copying it and nothing else, so
anything in it is part of what is being deleted, and a prefix would leave behind
exactly the objects whose keys this tool cannot predict.

**Objects first, records second.** A run that fails partway then leaves copies
recorded as `purged` with some of their objects gone, which is honest, because
`purged` never claimed the objects were there; recording first would claim a
deletion that had not happened. Deleting an object that is not there succeeds,
so an interrupted run is finished by running it again — and a dataset whose
objects are already gone reports that and exits `0` having deleted nothing.

**Example:**

```sh
$ data-releaser delete s3 --dataset-uri s3://source-bucket/datasets/cohort-a/
Deleting dataset s3://source-bucket/datasets/cohort-a/ empties the bucket its copy was staged into.

Deleted (1284 object(s), 4183920104 byte(s)):
  Bucket:  data-control-dataset-3f2a91c7b4e05d68 (1284 object(s), 4183920104 byte(s))

Recorded deleted (1 of 1 copy record(s)):
MECHANISM  ID                          ACCOUNT  SHARED                                 TARGET VPC  CREATED               EXPIRY  STATE
s3         01JQ8WBBBB0000000000000000  -        s3://source-bucket/datasets/cohort-a/  -           2026-07-20T01:00:00Z  -       purged

This reclaims storage and nothing else. Access to this dataset ended when it
was purged -- a key pending deletion decrypts nothing -- so what goes here is
ciphertext nobody could read, which the bucket's lifecycle rule would have
expired on its own before long. If the intent is to stop somebody reading, this
is the wrong command.
...

To delete these objects, type the dataset URI (s3://source-bucket/datasets/cohort-a/). Anything else abandons the deletion: s3://source-bucket/datasets/cohort-a/

Bucket emptied: data-control-dataset-3f2a91c7b4e05d68, 1284 object(s) deleted, 4183920104 byte(s) reclaimed

Objects deleted:
  Dataset:  s3://source-bucket/datasets/cohort-a/
  Bucket:   data-control-dataset-3f2a91c7b4e05d68, now empty (1284 object(s), 4183920104 byte(s) reclaimed)
  Copy:     01JQ8WBBBB0000000000000000 now deleted, recorded 2026-08-31T02:30:00Z

The storage is reclaimed and the objects are unrecoverable. The bucket is still
deployed, so staging this dataset again copies it back into the same place; the copy in
the external source store was never touched.
```

**Example — a copy somebody could still be reading:**

```sh
$ data-releaser delete s3 --dataset-uri s3://source-bucket/datasets/cohort-a/
Error: the objects of dataset "s3://source-bucket/datasets/cohort-a/" cannot be deleted
while 1 of its copies could still be read: 01JQ8WBBBB0000000000000000 is staged. They
share one bucket, so emptying it would take the data out from under whoever is reading
through those copies. Purge the dataset first -- that is what stops the reading -- and
delete its objects afterwards. Nothing was deleted
# exit status 1
```

Neither this refusal nor the one about a grant still deployed reads the bucket
at all, so a dataset that must not be emptied is refused without a listing of
thousands of objects first, and every copy standing in the way is named in one
pass rather than one per run.

---

## `list` — list copies, grants and shares

Report what is shared with whom, across both mechanisms, ordered by creation
date descending.

```
data-releaser list [s3|htsget] [--account <id>]
```

| Argument / flag | Required | Description |
| --- | --- | --- |
| `[s3\|htsget]` | no | Narrow the report to one mechanism. Both are covered when omitted. |
| `--account` | no | Filter to a single researcher AWS account ID. |

Every record is reported whatever state it is in, including revoked, expired,
and purged ones: the question is what has been released, not only what is
readable at this moment. For the `s3` mechanism that means **both kinds of
record** — the copies of a dataset held here and the grants of access to them,
since a copy nobody has been given is still something this account holds.

The two kinds are told apart by their columns. A **grant** row has an account, a
target VPC and an expiry; its `STATE` is `granted`, `revoked`, or the derived
`expired`. A **copy** row has none of those three and prints `-` in each,
carrying its own state instead — `copying`, `staged`, `failed`, `purged` or
`deleted`, which is what became of the transfer rather than whether anyone can
read. The two vocabularies are deliberately not folded into one word.

A `-` is not the same as an empty column. A copy names no researcher and no VPC
because who may read what it delivered is a grant's business, and it has no
expiry for the same reason: rendering that absence as `unlimited` would read as
unlimited access to a dataset nobody has been given at all. An expiry that is
genuinely absent on a *grant* prints `unlimited`.

For grants and htsget shares the `STATE` column shows the *effective* state
computed at read time, so a record whose expiry has passed reports as `expired`.
A copy's state is recorded rather than derived, so no clock is involved. One
clock is used across both mechanisms, so a record on either side of the expiry
boundary is not reported as expired in one half of the table and active in the
other.

The `ID` column is the operand `extend` and `revoke` take for that record — a
grant id or an htsget share id. `TARGET VPC` renders the bare `vpc-…` identifier
whichever form the record stored: an htsget share holds the full ARN and an s3
grant the bare identifier, and neither wants converting in storage, but the
column exists to be scanned down and compared. A value that is not a VPC ARN
prints exactly as it was stored, since a VPC written in the wrong shape is
precisely what a listing should let you notice.

`SHARED` is the library list for an htsget share (truncated after three IDs) or
the dataset URI for an s3 copy or grant.

**Example — list everything:**

```sh
$ data-releaser list
MECHANISM  ID                          ACCOUNT       SHARED                                 TARGET VPC             CREATED               EXPIRY                STATE
s3         01JQ8XABCDEF0123456789ABCD  777788889999  s3://source-bucket/datasets/cohort-a/  vpc-0123456789abcdef0  2026-08-01T04:00:00Z  2026-10-27T04:00:00Z  granted
s3         01JQ8XAAAA0000000000000000  111111111111  s3://source-bucket/datasets/cohort-a/  vpc-0aaaaaaaaaaaaaaa1  2026-08-01T03:00:00Z  unlimited             granted
s3         01JQ8WBBBB0000000000000000  -             s3://source-bucket/datasets/cohort-a/  -                      2026-07-20T01:00:00Z  -                     staged
s3         01JQ8WCCCC0000000000000000  -             s3://source-bucket/datasets/cohort-b/  -                      2026-07-19T01:00:00Z  -                     copying
htsget     01JQ7WZZZZ0000000000000000  777788889999  L2300009                               vpc-0123456789abcdef0  2026-06-01T00:00:00Z  unlimited             active
htsget     01JQ6VYYYY0000000000000000  111111111111  L2200001,L2200002,L2200003 (+2 more)   vpc-0aaaaaaaaaaaaaaa1  2026-05-01T00:00:00Z  2026-05-15T00:00:00Z  expired
```

The first three rows are one dataset: one copy, `staged`, and two researchers
reading it at the same time under grants of their own. Withdrawing either leaves
the other exactly as it is, and neither is a second transfer.

**Example — narrowed to one mechanism and account, with no matches:**

```sh
$ data-releaser list htsget --account 999999999999
No htsget shares found for account 999999999999.
```

The message names what was actually looked for, so an empty result cannot be
read as a broader question than the one that was asked: a bare run with nothing
recorded prints `No shares, copies or grants found.`, and `list s3` with nothing
recorded prints `No s3 copies or grants found.` All empty cases exit `0`.

Narrowing by `--account` leaves copies out altogether, which is correct: an
account filter asks what that account holds, and nobody holds a copy.

> `list` reads the ledger and nothing else. It reports what access was
> *intended*, and for the s3 mechanism that is not proof that the deployed
> policies agree with it — someone can edit a bucket or key policy in a
> console. Comparing the two is what `validate` is for.

---

## `validate` — validate the sharing infrastructure

Run preflight and reconciliation checks on the deployed infrastructure,
reporting a per-check pass/fail diagnostic. It is side-effect-free unless
`--test-setup` or `--reconcile` is given.

```
data-releaser validate [s3|htsget] [--account <id>] [--reconcile] [--test-setup]
```

| Argument / flag | Required | Description |
| --- | --- | --- |
| `[s3\|htsget]` | no | Narrow the checks to one mechanism. Both are checked when omitted. |
| `--account` | no | Restrict the checks to one researcher AWS account; this is also what enables the htsget researcher-side checks. |
| `--reconcile` | no | Replace the deployed bucket and key policies of every s3 dataset whose access differs from the ledger's. **This writes.** |
| `--test-setup` | no | Run canary test-setup mode: creates a short-lived synthetic share, invokes the researcher's test probe, and cleans up. Requires a configured canary. |

Each flag names work belonging to one mechanism, so accepting it against the
other would report on something that never ran: `--reconcile` is refused
alongside `validate htsget`, and `--test-setup` alongside `validate s3`.

In a bare run, a mechanism that is not configured at all is reported as a
failed check and the other mechanism's checks still run — which is the same
posture every other check here takes towards failure. Naming a mechanism makes
its configuration a hard requirement, since there is then nothing else to
report on.

**htsget checks.** For this mechanism the question is mostly reachability,
because the record itself is the enforcement point: the callout reads it per
request, so the record and reality cannot drift apart. The run covers
credential resolution (STS GetCallerIdentity), warehouse reachability (a
`SELECT 1 FROM mart.bam LIMIT 1` probe), the authorization service, htsget-rs
health via `service-info`, a cross-account canary HeadObject in the data
account, and the deployed ledger table, manifest bucket, and API Gateway stage.
With `--account` it adds the researcher-side checks: the VPC Lattice / RAM
association, the researcher's cross-account read grant on the manifest bucket,
and discovery and invocation of their test probe where one is deployed. With
`--test-setup` it adds the end-to-end canary.

**s3 checks.** Here the record and reality *can* drift, and routinely could:
access is two policy documents that AWS evaluates per request and that a person
can edit in a console. So the central check is a comparison. The run covers the
dataset ledger, the copy orchestration, the copy working bucket, the source
store's credentials secret and endpoint, then **one comparison per dataset**
between the access the ledger describes and the access its deployed documents
actually grant, then the state of every key a dataset is encrypted under.

The comparison is per dataset, and deliberately not per copy or per grant. The
bucket and the key are deployed per dataset and hold one pair of documents
between them, so every copy of that dataset and every grant made against those
copies is enforced by that same pair. Comparing against a pair rendered from any
smaller unit — one copy of a dataset staged twice, one grant of a dataset given
to two researchers — would report everything the rest of the ledger describes as
drift nobody accounts for, and reconciliation would then "repair" it by writing
that pair over grants doing exactly what the ledger asks.

Every grant recorded against a dataset's copies goes into the comparison
whatever state it is in, and the withdrawn ones are the interesting ones: a
grant still deployed after its revocation is access nobody believes exists. The
comparison is skipped where a dataset's key has already been destroyed, since
what was copied is unreadable for good and there is no access left to compare.

**`--reconcile` is the one read-only-looking command that writes.** For every
s3 dataset whose deployed access differs from the ledger's, it replaces both
documents with the pair the ledger describes. It writes them whole, so a
statement added by hand is gone afterwards and one removed by hand is back, and
what it replaced is reported — a repair you cannot see the shape of is not a
repair you can check. It is never implied by a plain validation run, and it is
skipped for a dataset whose deployed policies could not be read at all, since
writing there would replace documents whose contents are unknown. Note the
direction: reconciliation treats the *ledger* as the truth, so a grant that is
live but unrecorded is taken back out, and a revocation that was applied but
not recorded would be undone. Fix the record first where a failure said so.

When `--account` narrows the run, the narrowing is by *dataset* rather than by
grant: every copy and every grant belonging to a dataset that account holds a
grant against is kept. A pair rendered from one account's grants alone would be
missing every other account's statements, so the comparison would report each of
those as drift and reconciliation would then write that pair and withdraw access
nobody asked about.

Every check runs to completion even if earlier ones fail — a deployment with
three problems should take one run to discover, not three. The command exits
non-zero if **any** check failed, a failed repair included. Skipped checks
render as `SKIP` and are excluded from the summary's denominator, so a healthy
deployment does not look partially broken.

**Example (abridged):**

```sh
$ data-releaser validate --account 777788889999
htsget mechanism:
─────────────────
  [PASS] Credential Resolution (STS GetCallerIdentity)
  [PASS] OrcaVault Mart Reachability (Athena)
  [PASS] Authorization Service Reachability
  [PASS] Cross-Account S3 Access (Data Account)
  [PASS] VPC Lattice / RAM Association

s3 mechanism:
─────────────
  [PASS] Dataset ledger readable (data-releaser-ShareRecords)
  [PASS] Copy orchestration reachability (arn:aws:states:ap-southeast-2:999988887777:stateMachine:steps-s3-copy)
  [PASS] Copy working bucket writability (steps-s3-copy-working)
  [PASS] Source store credentials secret (source-store/credentials)
  [PASS] Source store endpoint (https://ceph.example.org)
  [FAIL] Deployed access matches the ledger: s3://source-bucket/datasets/cohort-a/
         the deployed bucket policy grants account 111111111111, which the ledger does not describe
  [PASS] Dataset key state: s3://source-bucket/datasets/cohort-a/

Summary: 11/12 checks passed
# exit status 1
```

The two per-dataset checks name their dataset because there is one of each per
dataset; the five that stand alone name the resource they are about instead.

The htsget section above is abridged: a real run also reports the htsget-rs
health endpoint, the deployed ledger table, the manifest bucket, the API Gateway
stage, the researcher's cross-account read grant on the manifest bucket, and —
with a probe deployed in the researcher account — its discovery and invocation.

Where reconciliation repairs something, the repairs are printed in a block of
their own, ahead of the s3 check section, because each of them wrote to a
bucket or a key policy and an operator reading the output should not have to
work out which of the two a line is:

```sh
$ data-releaser validate s3 --reconcile
s3 mechanism, policies re-applied from the ledger:
──────────────────────────────────────────────────
  [PASS] Ledger policies re-applied: s3://source-bucket/datasets/cohort-a/
         replaced the key policy of arn:aws:kms:... and the bucket policy of
         data-control-dataset-3f2a91c7b4e05d68. The deployed access is now what the ledger
         describes: account 777788889999 reading from vpc-0123456789abcdef0, until
         2026-10-27T04:00:00Z, and account 111111111111 reading from
         vpc-0aaaaaaaaaaaaaaa1, with no expiry
```

A repair says who it left reading rather than only how many, and it names them
in the order the documents state them. Where the ledger describes no live grant
at all, it says that instead: neither document grants any researcher access
now, which is what the ledger describes.

Run `validate` after deploying the infrastructure and before onboarding a
researcher, to confirm the system is healthy independently of creating a real
production share. Run it with `s3` periodically thereafter, because that is the
mechanism whose deployed access can be changed without this tool noticing.
