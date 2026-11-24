# Researcher manifests

A manifest is a JSON document this tool writes for a researcher account, describing the
access that account holds. There are two of them, one per release mechanism. The htsget
manifest lists the library IDs and files a researcher may request through htsget-rs, so
they can construct valid requests. The dataset manifest lists the copies of datasets a
researcher may read directly out of S3, and where to read them from. This page documents
both schemas and where each is written, and states explicitly that neither is ever
consulted when a read is authorised.

---

## Descriptive only — never an authorization surface

Both documents are tool-generated projections. They reflect access that has been granted;
they do not grant it.

- Access is enforced elsewhere, and computed from the ledger rather than from a manifest.
  For htsget it is the authorization callout, which reads the share records for each
  request it is asked about; for the s3 mechanism it is the cache bucket's policy and the
  policy of the key encrypting it, which AWS evaluates on every request. Neither path
  opens a manifest.
- Consequently, **editing a manifest has no effect on access**. A researcher cannot widen
  their access by rewriting their copy, and the CLI overwrites the object whole on every
  regeneration.
- That is a constraint on the schema as much as on the code. A manifest carries where the
  data is, how to ask for it, and when access ends — what a reader needs in order to find
  the data — and deliberately carries no permissions and no policy fragments, so there is
  nothing in it that invites treating it as the authority on what may be done.
- A manifest describes the access held at the moment it was generated and nothing else.
  Access that has been withdrawn is simply absent, and an account with none left has no
  manifest at all: publishing removes the object rather than leaving behind a description
  of access that has ended.

The one thing to watch is that a manifest is only rewritten when a command runs. Expiry is
derived at read time from the record rather than swept by anything, so between a grant
expiring and the next command for that account, its manifest still lists the grant — with
an `expiresAt` in the past. Enforcement has already stopped honouring it; only the
description is behind.

Researchers should treat a manifest as a directory of what they can reach, not as a
control surface.

---

## Two documents, two prefixes

Both mechanisms deliver through a bucket of the same shape, and by default through the
same bucket. They differ in the key prefix, deliberately: one researcher may hold both
kinds of access at once, and neither description should overwrite the other.

| | htsget manifest | dataset manifest |
| --- | --- | --- |
| Bucket setting | `manifest.bucket` | `s3.manifest.bucket` (defaults to `manifest.bucket`) |
| Key setting | `manifest.keyTemplate` | `s3.manifest.keyTemplate` |
| Default key | `manifests/{accountId}/manifest.json` | `datasets/{accountId}/manifest.json` |
| Built from | the account's active share records | the grant ledger — every active grant the account holds |
| Republished by | `share htsget`, `extend htsget`, `revoke htsget` | `share s3`, `extend s3`, `revoke s3`, `purge s3` |

`{accountId}` is the only placeholder either template supports. Any other `{...}` token is
rejected when the configuration section is checked, so a misconfigured template fails
before a researcher's manifest is first written rather than at the moment of writing it.

- **Bucket.** A single manifest bucket in the account this tool runs in, provisioned by
  the CDK stack and named from its `ManifestBucketName` output rather than hard-coded. It
  is not a per-researcher bucket. See `docs/configuration.md`.
- **Write path.** The CLI writes the object as a **same-account** write using the Data
  Control identity (`s3:PutObject`), and deletes it (`s3:DeleteObject`) when the account
  has no access of that kind left. It never creates the bucket or edits its policy; that
  belongs to the stack.
- **Read path.** Researchers read their own manifest **cross-account**. That needs a grant
  on both sides — the bucket policy on the Data Control side and an IAM policy in the
  researcher's own account. The stack writes one bucket-policy statement per onboarded
  account covering both prefixes at once. See `docs/onboarding.md`.

---

## The htsget manifest

The top-level document contains the htsget-rs base URL, the generation timestamp
(ISO 8601 / RFC 3339, UTC), and a `libraries` array.

| Field | Type | Notes |
| --- | --- | --- |
| `htsgetBaseUrl` | string | The htsget-rs base URL, so the researcher can construct valid request URLs. Taken from `endpoints.htsgetBaseUrl`. |
| `generatedAt` | string (ISO 8601) | When the manifest was generated (UTC). |
| `libraries` | array | One entry per library the account can currently access, sorted by `libraryId`. |

Each entry in `libraries`:

| Field | Type | Notes |
| --- | --- | --- |
| `libraryId` | string | The library identifier. |
| `expiresAt` | string (ISO 8601) or `null` | The **effective** expiry for the library: the latest expiry among the active shares granting it, or `null` (unlimited) if any granting share is unlimited. |
| `files` | array | The accessible files for this library, deduplicated and sorted by request path. |

Each entry in a library's `files`:

| Field | Type | Notes |
| --- | --- | --- |
| `request` | string | The htsget request path relative to `htsgetBaseUrl`, encoding the reads endpoint. Variants (VCF/BCF) are out of current scope. |
| `rules` | array (optional) | Omitted or empty means **unrestricted** read access to that file. When present, access is the union of the listed rules. |

The request path is `reads/<htsget id>`, where the id is the object's S3 key without its
bucket and without the format extension — for example `reads/demo/L2400001/NA12878`. The
library ID cannot stand in for it: htsget-rs resolves an id against an S3 key, and a
library maps to one or more objects whose keys are unrelated to its ID, so
`reads/<library id>` names nothing that can be fetched. The files are recorded on the
share at share time purely so the manifest can name these paths; authorization is granted
on the prefixes, never on this list. A share written before file recording existed
contributes nothing here, and its library then appears with an empty `files` array rather
than a fabricated path that would 404.

Each rule mirrors the htsget-rs `ReferenceNameRestriction`. The same sub-schema is used in
the authorization callout response, so the researcher-facing view matches exactly what
htsget-rs enforces. An absent field means that dimension is unrestricted:

| Field | Type | Notes |
| --- | --- | --- |
| `referenceName` | string (optional) | Restrict to a reference name; absent means all. |
| `format` | string (optional) | One of `BAM`, `CRAM`, `VCF`, `BCF`; absent means all. |
| `start` | integer (optional) | 0-based inclusive start; absent means from the beginning. |
| `end` | integer (optional) | 0-based exclusive end (`[start, end)`); absent means to the end. |

### Example

The current design grants whole-library access, so the CLI emits no `rules` today. The
structure is shown on the second library for completeness:

```json
{
  "htsgetBaseUrl": "https://htsget.example.internal",
  "generatedAt": "2024-01-15T04:05:06Z",
  "libraries": [
    {
      "libraryId": "L2400123",
      "expiresAt": "2024-02-14T04:05:06Z",
      "files": [
        { "request": "reads/demo/L2400123/NA12878" }
      ]
    },
    {
      "libraryId": "L2400456",
      "expiresAt": null,
      "files": [
        {
          "request": "reads/demo/L2400456/NA12879",
          "rules": [
            { "referenceName": "chr1", "format": "BAM", "start": 1000, "end": 2000 }
          ]
        }
      ]
    }
  ]
}
```

In this example `L2400123` expires on 14 Feb 2024, while `L2400456` is unlimited
(`expiresAt: null`) because at least one granting share has no expiry.

Because a share's library set is immutable (see `docs/operations.md`), this document stays
a convergent, drift-free projection: it is rebuilt from the account's whole share list on
every create, extension and revocation, and always reflects the remaining active grants.

---

## The dataset manifest

This is the document for the s3 mechanism, and it is built from the **grant** ledger
rather than the copy ledger. A copy is one transfer of a dataset into this account and
says nothing about who may read what it delivered; a grant is exactly what the document
has to describe — one account, one dataset, one VPC, one end date — and it carries the
bucket and the key alongside, so an entry is written from one record without a join.

| Field | Type | Notes |
| --- | --- | --- |
| `note` | string | A fixed caution saying what the document is and is not. It travels inside the payload rather than sitting in a comment somewhere, because the document outlives the conversation in which it was handed over and will be read by people who were not part of it. |
| `accountId` | string | The account the document was written for, so a reader can tell at a glance that they are looking at their own. |
| `generatedAt` | string (ISO 8601) | When the document was built (UTC). Everything below describes the access held at that moment and nothing about what it will be later. |
| `datasets` | array | One entry per grant currently giving this account access, sorted by `datasetUri` then `grantId`. |

Each entry in `datasets`:

| Field | Type | Notes |
| --- | --- | --- |
| `grantId` | string | Identifies the access this entry describes. It is the identifier to quote when asking about that access, which is the only thing it is good for — an operator needs it, a reader's tooling does not. |
| `datasetUri` | string | Names the dataset in the source store it was copied from. This is the dataset's identity, not somewhere the reader can reach. |
| `bucket` | string | The cache bucket holding the readable copy. |
| `region` | string | Where that bucket is served from. A reader needs both it and the bucket to address the data. |
| `prefix` | string | Where the dataset's objects sit in the bucket. Empty means the bucket root, which is where they sit today: a cache bucket holds exactly one dataset, and the copy reproduces the dataset's own folder structure from there. |
| `excludedPrefix` | string | A prefix inside the bucket that is **not** part of the dataset and is not readable by this account — it holds the record of how the copy ran. It appears in a listing of the bucket, so naming it here saves a reader working out why fetching from it is refused. |
| `keyArn` | string | The key the objects are encrypted with. Reads are decrypted per request, so a reader whose client cannot use this key sees a refusal rather than an object. |
| `targetVpcId` | string | The bare VPC identifier reads must originate from. Requests reaching the bucket from anywhere else are refused, and that refusal looks like every other one, so it is worth stating. |
| `expiresAt` | string (ISO 8601) or `null` | When access ends. `null` means it does not end on a date, which is not the same as permanent: access can still be withdrawn. |

There is one entry per **grant**, not per dataset. Two grants of the same dataset to the
same account can differ in the VPC they are read from and in when they end, and they may
even read from different copies of it, so collapsing them into one entry would have to
misreport one or the other.

### Example

```json
{
  "note": "Descriptive only: this document records the dataset access held by this account at the time it was generated. Access is enforced by the cache bucket's policy and by the policy of the key encrypting it. Nothing here is consulted when a read is authorised, and changing this document neither grants nor withdraws anything.",
  "accountId": "777788889999",
  "generatedAt": "2024-01-15T04:05:06Z",
  "datasets": [
    {
      "grantId": "01JQ8XABCDEF0123456789ABCD",
      "datasetUri": "s3://source-bucket/datasets/cohort-a/",
      "bucket": "data-releaser-dataset-3f2a91c7b4e05d68",
      "region": "ap-southeast-2",
      "prefix": "",
      "excludedPrefix": "_control/",
      "keyArn": "arn:aws:kms:ap-southeast-2:999988887777:key/8c1d0f42-1b6e-4a7f-9d3c-52ea0b7f6a10",
      "targetVpcId": "vpc-0123456789abcdef0",
      "expiresAt": "2024-02-14T04:05:06Z"
    }
  ]
}
```

### Publishing semantics

The delivery target is resolved **before** a command touches anything, because publishing
happens last: a target that cannot be resolved is a configuration mistake, and discovering
it after the policies are in force turns a mistake the operator could have fixed before
starting into one they have to chase.

Publishing then runs after the two policy documents have been written and after the ledger
has recorded what they say. It is the only step whose failure leaves nothing dangerous
behind — the access is exactly what was asked for either way, and what is stale is a
description of it — so the change stands and the failure is reported rather than anything
being unwound. It is still reported as a failure rather than a warning: the document is
what a researcher navigates by, and a stale one either advertises access that has ended or
omits access that has begun, neither of which anything else in this tool will notice,
since nothing reads the document back. Re-running the same command republishes it; the
document is rebuilt from the ledger every time rather than edited, so nothing further is
changed by doing so.

The document is always rebuilt from every grant the account holds, never from whichever
grant just changed: one account has one document covering all of its access, so a document
derived from a single change would drop the rest of it. Grants held by another account are
dropped rather than reported — the document is readable by the account it was written for,
so including them would disclose someone else's access.

---

## htsget-rs callout caching contribution

The authorization response caching that bounds revocation propagation (see
`docs/operations.md`) is not something this repository can deliver on its own. The
authorization service here already emits the `Cache-Control` directives that govern it,
and `cache.ttlCeiling` already caps the `max-age` those directives advertise, but the
cache that honours them lives in htsget-rs. This section records the upstream change the
bound depends on, so the link between the two is traceable.

- **Target repository:** `umccr/htsget-rs`.
- **Target crates / modules:**
  - `htsget-config` — the callout/auth configuration layer:
    `htsget-config/src/config/advanced/callout.rs` (gains a `cache = { enabled, max_ttl }`
    option on the `Callout` type),
    `.../auth/authorization.rs`, and `.../auth/response.rs` (the `AuthorizationRestrictions`
    type the cache stores).
  - `htsget-http` — the HTTP client / auth middleware that performs the callout:
    `htsget-http/src/middleware/auth.rs` (`Auth::query_authorization_service` /
    `Auth::fetch_from_callout`), where the response cache wraps the callout and
    `Cache-Control`/`Age` headers are read before the body is consumed.
- **Configuration it introduces:** a `cache` block on the callout config exposing an
  enable/disable toggle and a maximum TTL ceiling that caps any `max-age` returned by the
  authorization service; a missing `Cache-Control` directive is treated as non-cacheable.
  The cache is keyed on the callout URL plus the forwarded header set (deployment-neutral,
  but carrying the requester identity), preserving per-identity isolation.

The revocation window described in `docs/operations.md` is therefore a property of the
deployed htsget-rs rather than of anything in this repository. Where the callout client
holds no cache, the ceiling bounds nothing and every request is authorised from a freshly
computed decision — the safe direction to be wrong in, since revocation is then observed
sooner rather than later, but not a window anything here can verify. Link the merged
`umccr/htsget-rs` pull request here once the contribution lands, and confirm the deployed
htsget-rs carries it before relying on the bound.
