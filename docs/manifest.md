# The Manifest

The Manifest is a JSON document that tells a researcher which library IDs and files they
have been granted access to, so they can construct valid htsget-rs requests. This page
documents its schema and where it is written, and states explicitly that it is
descriptive only and is **never** consulted for authorization (Req 18.8, 7.8).

---

## Descriptive only — never an authorization surface

The Manifest is a tool-generated, descriptive artifact. It reflects the access a
Researcher_Account has been granted, but it does **not** itself grant access:

- Access is enforced **server-side** by the Authorization_Service and htsget-rs, computed
  from the Share_Records alone. The Manifest is never read when making an authorization
  decision (Req 7.8).
- Consequently, **modifying the manifest has no effect on access**. A researcher cannot
  widen their access by editing their copy, and the Data_Releaser may freely overwrite
  the object on any regeneration.
- The Manifest is a pure projection of the currently active Share_Records for an account.
  Because a share's library set is immutable (see `docs/operations.md`), the manifest
  stays convergent and drift-free: it is regenerated on create/extend/revoke/expiry and
  always reflects exactly the remaining active grants.

Researchers should treat the manifest as a directory of what they can query, not as a
control surface.

## Location

- **Bucket:** the single Data Control **Manifest_Bucket**, owned and provisioned by this
  project's Infrastructure_Stack (Req 7.9, 16.5). It is not a per-researcher bucket.
- **Key:** one manifest per Researcher_Account under a per-account prefix —
  `manifests/<researcher-account-id>/manifest.json` by default.
- **Configuration:** the bucket name and key template are resolved from configuration,
  never hard-coded. `manifest.bucket` is sourced from an Infrastructure_Stack output, and
  `manifest.keyTemplate` defaults to `manifests/{accountId}/manifest.json` (the only
  supported placeholder is `{accountId}`). See `docs/configuration.md`.
- **Write path:** the Data_Releaser writes the object as a **same-account** write using
  the Data Control identity (`s3:PutObject`), and deletes it (`s3:DeleteObject`) when no
  active shares remain for the account (Req 7.3, 7.4).
- **Read path:** researchers read their own manifest **cross-account**. This requires a
  grant on both sides — the Manifest_Bucket policy (Data Control side) and the
  researcher's own IAM policy (researcher side). See `docs/onboarding.md`.

## JSON schema

The top-level document contains the htsget-rs base URL, the generation timestamp
(ISO 8601 / RFC 3339, UTC), and a `libraries` array (Req 7.6).

| Field | Type | Notes |
| --- | --- | --- |
| `htsgetBaseUrl` | string | The htsget-rs base URL, so the researcher can construct valid request URLs (Req 7.5). |
| `generatedAt` | string (ISO 8601) | When the manifest was generated (UTC). |
| `libraries` | array | One entry per library the account can currently access. |

Each entry in `libraries`:

| Field | Type | Notes |
| --- | --- | --- |
| `libraryId` | string | The library identifier. |
| `expiresAt` | string (ISO 8601) or `null` | The **effective** expiry for the library: the latest expiry among the active shares granting it, or `null` (unlimited) if any granting share is unlimited (Req 7.7). |
| `files` | array | The accessible files for this library. |

Each entry in a library's `files`:

| Field | Type | Notes |
| --- | --- | --- |
| `request` | string | The htsget request path relative to `htsgetBaseUrl`, encoding the reads endpoint — e.g. `reads/<library_id>`. Variants (VCF/BCF) are out of current scope. |
| `rules` | array of Access_Rule (optional) | Omitted or empty means **unrestricted** read access to that file. When present, access is the union of the listed rules. |

Each Access_Rule (mirrors the htsget-rs `ReferenceNameRestriction`; the same sub-schema
is used in the Authorization_Service callout response, so the researcher-facing view
matches exactly what htsget-rs enforces). An absent field means that dimension is
unrestricted:

| Field | Type | Notes |
| --- | --- | --- |
| `referenceName` | string (optional) | Restrict to a reference name; absent means all. |
| `format` | string (optional) | One of `BAM`, `CRAM`, `VCF`, `BCF`; absent means all. |
| `start` | integer (optional) | 0-based inclusive start; absent means from the beginning. |
| `end` | integer (optional) | 0-based exclusive end (`[start, end)`); absent means to the end. |

### Example

The current design grants whole-library access, so `files` entries carry no `rules`
(unrestricted). The `rules` structure is shown for completeness:

```json
{
  "htsgetBaseUrl": "https://htsget.example.internal",
  "generatedAt": "2024-01-15T04:05:06Z",
  "libraries": [
    {
      "libraryId": "L2400123",
      "expiresAt": "2024-02-14T04:05:06Z",
      "files": [
        { "request": "reads/L2400123" }
      ]
    },
    {
      "libraryId": "L2400456",
      "expiresAt": null,
      "files": [
        {
          "request": "reads/L2400456",
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

---

## htsget-rs callout caching contribution (Requirements 14, 18.9)

The authorization response caching that bounds revocation propagation (see
`docs/operations.md`) depends on an upstream change to htsget-rs. This dependency is
recorded here so the link between this project and the upstream change is traceable
(Req 18.9).

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
  Authorization_Service; a missing `Cache-Control` directive is treated as non-cacheable.
  The cache is keyed on the callout URL plus the forwarded header set (deployment-neutral,
  but carrying the requester identity), preserving per-identity isolation.
- **Prototype:** a standalone prototype lives in this repo's `htsget-cache/` crate; it is
  **not** integrated into htsget-rs and does not by itself satisfy Requirement 14. It is
  deleted once the upstream PR lands.

> **TODO (task 15.8):** link the merged `umccr/htsget-rs` pull request here once the
> callout-caching contribution lands. Do not treat this contribution as complete until
> that PR is merged; the prototype `htsget-cache/` crate is then removed from this repo.
