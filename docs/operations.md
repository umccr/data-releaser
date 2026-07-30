# Operational Characteristics

This page documents the behaviours an administrator must understand to operate the
Data Releaser safely (Req 18.7). None of them are bugs — they are consequences of the
caching model, the immutable-share model, and the warehouse-backed resolution described
in `design.md`.

---

## Revocation is bounded by the Cache_TTL, not immediate

`revoke <share-id>` marks the Share_Record as revoked immediately: the Authorization_Service
stops returning that share in **freshly computed** authorization responses right away
(Req 3.1, 3.2). But htsget-rs does not call the Authorization_Service on every request —
authorization decisions are cached (Req 13), so a researcher can continue to be authorized
from a cached decision until that cache entry expires.

The staleness bound is:

```
min(share's remaining lifetime at cache time, Cache_TTL ceiling)
```

For unlimited or long-lived shares this equals the ceiling. The ceiling is configurable
(`cache.ttlCeiling`, default **1 hour**, maximum 24 hours; see `docs/configuration.md`).
So after a revoke, plan for access to persist for up to the ceiling.

There is exactly **one** cache: the HTTP response cache on the htsget-rs authorization
callout client. It is governed entirely by the RFC 9111 response directives the
Authorization_Service emits with each decision — `Cache-Control` with `s-maxage` and
`public` for a decision that may be reused, `no-store` for one that must not be, plus
`Vary` so responses stay isolated per requester identity. A response carrying no
`Cache-Control` directive is not stored at all.

Because that cache is a standards-governed HTTP cache living inside the htsget-rs process,
**there is nothing an operator can invalidate from the outside**: no flush operation, no
eviction API, and no override. Stored decisions leave the cache only by expiring.

Because effective status is computed at read time from `expiresAt`/`revokedAt`, a
revoked share is never re-authorized once cached decisions have expired — the revocation is
durable; only its *observation* by htsget-rs is delayed.

## Revocation propagation

Revocation happens in two distinct steps, and only the first is under operator control:

- **The revocation itself is immediate and durable.** `revoke` writes `revokedAt` to the
  Share_Record and regenerates (or deletes) the researcher's manifest. Once that write
  returns, the share is revoked permanently — nothing reactivates it, and every
  freshly computed decision denies it from that moment on.
- **Observation by htsget-rs is delayed.** A decision already stored in the callout
  response cache keeps being served until it expires, bounded by the Cache_TTL ceiling.

There is **no manual flush**. `revoke` takes no cache-invalidation flag, the
Authorization_Service exposes no eviction operation, and the cache honours only the
directives it was given when the response was stored. Every `revoke` therefore prints an
unconditional note that propagation is bounded by the Cache_TTL ceiling — the bound always
applies, so the note is not conditional on anything.

The consequence is that **the ceiling *is* the revocation SLA**: it is the longest a
revoked researcher may retain access. Choose it as a deliberate security decision, not as a
performance tuning knob. Lowering `cache.ttlCeiling` shortens the revocation window
proportionally, at the cost of more callouts to the Authorization_Service (a lower hit rate
means more Lambda invocations per request stream). Raising it buys fewer callouts and pays
for them with a longer window in which an already-revoked grant is still honoured.

> Correctness never depends on the cache being present — a miss simply falls back to a
> fresh callout (Req 13.10), which recomputes the current, now-denying decision.

## Library sets are immutable — change means revoke and recreate

A Share_Record's library set is **immutable** (Req 3.9). The CLI provides no way to edit
which libraries a share covers — `share` has no in-place-edit mode, and `extend` only
changes the expiry.

To change which libraries a Researcher_Account can access:

- **To add libraries:** create a new share with the additional libraries. Access is
  compositional and the manifest unions across all active shares for the account, so the
  researcher simply gains the new libraries.
- **To remove a subset:** you cannot shrink an existing share. Revoke the affected share
  and create a new share with the desired (smaller) set — "revoke and recreate". Mind the
  revocation propagation window above while the old grant drains from the cache.

This immutability is what keeps the manifest a pure, drift-free projection of active
Share_Records (`docs/manifest.md`).

## Share creation is idempotent

Re-running the same `share` command does not create duplicates (Req 12):

- **Exact match** — same set of library IDs (matched **regardless of order**) and the
  same target Researcher_Account as an existing **active** share: the CLI returns the
  existing share identifier and prints that an existing share was reused, rather than
  creating a duplicate (Req 12.1, 12.2).
- **Different requested expiry, same set + account:** the existing share is still reused
  and its expiry is **retained unchanged** — the CLI does not modify the existing share's
  expiry (Req 12.3). Use `extend` if you actually want to change an expiry.
- **Partial overlap** — the library set overlaps an existing active share but is not an
  exact set match: a **new** share is created with the requested set (Req 12.4).

Idempotency is matched only against **active** shares; a revoked or expired share with
the same set does not block a fresh create.

## Warehouse resolution can lag live OrcaBus state by ~1 day

Library resolution reads the OrcaVault Mart (`mart.bam`) via a single Athena query over
the Lake Formation share — not the live OrcaBus service APIs. OrcaVault is a downstream,
daily-batch-refreshed warehouse, so resolution reflects the state of the **most recent
daily build** and may lag live OrcaBus state by up to roughly a day.

Practical consequences:

- A library that was aligned very recently may not yet appear in `mart.bam`. Until the
  next daily build lands, `share` reports it as having no shareable reads (the message
  notes the ID may not exist, may not be aligned yet, or may have reads that are
  archived) and aborts without creating partial records (Req 1.1, 1.6).
- This lag is an accepted tradeoff and is fine for sharing already-produced data. If a
  share fails for a library you know exists, waiting for the next daily build is often
  the resolution.
- Only aligned-read files are covered today (`mart.bam`, `format = 'bam'`, plus the
  co-located `.bai` index). Variant files (VCF/BCF) are out of scope until a
  corresponding warehouse mart exists.
- Archived reads are not shareable: if a resolved file reports an archived
  `storage_class` (e.g. a Glacier/DeepArchive tier), `share` reports the affected library
  IDs and locations and aborts, since htsget-rs could not stream them without a restore
  (Req 1.7).
