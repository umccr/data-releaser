# Operational Characteristics

This page documents the behaviours an administrator must understand to operate
Data Releaser safely. None of them are bugs — they are consequences of the
caching model, the immutable-share model, the warehouse-backed resolution, and
the fact that the two mechanisms enforce access by entirely different means.

The first four sections below are about the **htsget** mechanism, where the
record is read on every request. The last is about the **s3** mechanism, where
it is not.

---

## htsget revocation is bounded by the cache TTL, not immediate

`revoke htsget <share-id>` marks the share record as revoked immediately: the
authorization service stops returning that share in **freshly computed**
authorization responses right away. But htsget-rs does not call the
authorization service on every request — authorization decisions are cached, so
a researcher can continue to be authorized from a cached decision until that
cache entry expires.

None of this applies to the s3 mechanism, where there is no callout and nothing
of ours in the read path: `revoke s3` takes the granting statements out of the
bucket policy, and AWS evaluates that policy per request, so reads stop as the
document is written.

The staleness bound is:

```
min(share's remaining lifetime at cache time, cache TTL ceiling)
```

For unlimited or long-lived shares this equals the ceiling. The ceiling is configurable
(`cache.ttlCeiling`, default **1 hour**, maximum 24 hours; see `docs/configuration.md`).
So after a revoke, plan for access to persist for up to the ceiling.

There is exactly **one** cache: the HTTP response cache on the htsget-rs authorization
callout client. It is governed entirely by the RFC 9111 response directives the
authorization service emits with each decision — `Cache-Control` with `s-maxage` and
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

- **The revocation itself is immediate and durable.** `revoke htsget` writes `revokedAt`
  to the share record and regenerates (or deletes) the researcher's manifest. Once that
  write returns, the share is revoked permanently — nothing reactivates it, and every
  freshly computed decision denies it from that moment on.
- **Observation by htsget-rs is delayed.** A decision already stored in the callout
  response cache keeps being served until it expires, bounded by the cache TTL ceiling.

There is **no manual flush**. `revoke htsget` takes no cache-invalidation flag, the
authorization service exposes no eviction operation, and the cache honours only the
directives it was given when the response was stored. Every `revoke htsget` therefore
prints an unconditional note that propagation is bounded by the cache TTL ceiling — the
bound always applies, so the note is not conditional on anything.

The consequence is that **the ceiling *is* the revocation SLA**: it is the longest a
revoked researcher may retain access. Choose it as a deliberate security decision, not as a
performance tuning knob. Lowering `cache.ttlCeiling` shortens the revocation window
proportionally, at the cost of more callouts to the authorization service (a lower hit rate
means more Lambda invocations per request stream). Raising it buys fewer callouts and pays
for them with a longer window in which an already-revoked grant is still honoured.

> Correctness never depends on the cache being present — a miss simply falls back to a
> fresh callout, which recomputes the current, now-denying decision.

## Library sets are immutable — change means revoke and recreate

A share record's library set is **immutable**. The CLI provides no way to edit which
libraries a share covers — `share htsget` has no in-place-edit mode, and `extend htsget`
only changes the expiry.

To change which libraries a researcher account can access:

- **To add libraries:** create a new share with the additional libraries. Access is
  compositional and the manifest unions across all active shares for the account, so the
  researcher simply gains the new libraries.
- **To remove a subset:** you cannot shrink an existing share. Revoke the affected share
  and create a new share with the desired (smaller) set — "revoke and recreate". Mind the
  revocation propagation window above while the old grant drains from the cache.

This immutability is what keeps the htsget manifest a pure, drift-free projection of the
account's active share records (`docs/manifest.md`).

## htsget share creation is idempotent; an s3 grant is not

Re-running the same `share htsget` command does not create duplicates:

- **Exact match** — same set of library IDs (matched **regardless of order**) and the
  same target researcher account as an existing **active** share: the CLI returns the
  existing share identifier and prints that an existing share was reused, rather than
  creating a duplicate.
- **Different requested expiry, same set + account:** the existing share is still reused
  and its expiry is **retained unchanged** — the CLI does not modify the existing share's
  expiry. Use `extend htsget` if you actually want to change an expiry.
- **Partial overlap** — the library set overlaps an existing active share but is not an
  exact set match: a **new** share is created with the requested set.

Idempotency is matched only against **active** shares; a revoked or expired share with
the same set does not block a fresh create.

`share s3` behaves differently and deliberately so. Every invocation mints a **new grant**
with an identifier of its own, so a researcher whose access was withdrawn and is then given
back holds a second grant while the withdrawn one keeps its record and its revocation
timestamp. What is refused is not a repeat but a *duplicate*: the same account reading the
same copy from the same VPC twice, which would deploy two statements saying the same thing
and leave two records where one describes the access. The refusal names the standing grant
and says to move its expiry or withdraw it. Granting the same account access from a
different VPC, or another account access to the same copy, is a separate grant and is not
refused.

## Warehouse resolution can lag live OrcaBus state by ~1 day

This applies to the htsget mechanism only; the s3 mechanism names a dataset by URI
and never consults the warehouse.

Library resolution reads the OrcaVault Mart (`mart.bam`) via a single Athena query over
the Lake Formation share — not the live OrcaBus service APIs. OrcaVault is a downstream,
daily-batch-refreshed warehouse, so resolution reflects the state of the **most recent
daily build** and may lag live OrcaBus state by up to roughly a day.

Practical consequences:

- A library that was aligned very recently may not yet appear in `mart.bam`. Until the
  next daily build lands, `share htsget` reports it as having no shareable reads (the
  message notes the ID may not exist, may not be aligned yet, or may have reads that are
  archived) and aborts without creating partial records.
- This lag is an accepted tradeoff and is fine for sharing already-produced data. If a
  share fails for a library you know exists, waiting for the next daily build is often
  the resolution.
- Only aligned-read files are covered today (`mart.bam`, `format = 'bam'`, plus the
  co-located `.bai` index). Variant files (VCF/BCF) are out of scope until a
  corresponding warehouse mart exists.
- Archived reads are not shareable: if a resolved file reports an archived
  `storage_class` (e.g. a Glacier/DeepArchive tier), `share htsget` reports the affected
  library IDs and locations and aborts, since htsget-rs could not stream them without a
  restore.

---

## s3 access can drift from the ledger, and only `validate` notices

This is the characteristic with no counterpart on the htsget side, and it comes
from the same property that makes the s3 mechanism attractive: nothing of ours
is in the read path. Access there is two policy documents — the cache bucket's
and the policy of the key encrypting it — that AWS evaluates on every request
and that a person can edit in a console, with an SDK, or from another stack. The
ledger records the access that was *intended*; the deployed documents are what a
request is actually checked against, and the two can disagree.

`list` reads only the ledger, so it cannot answer whether they agree. Comparing
them is what `validate` is for, and it is worth running periodically against the
s3 mechanism for exactly this reason:

- `data-releaser validate s3` compares, once per dataset, the access the ledger
  describes against the access its deployed documents grant, and reports each
  difference. The states that grant nothing are the interesting ones: a grant
  still deployed after its revocation is access nobody believes exists.
- `data-releaser validate s3 --reconcile` **writes**. It replaces both documents
  of every dataset that differs with the pair the ledger describes, whole — so a
  statement added by hand is gone afterwards and one removed by hand is back.

Reconciliation treats the **ledger** as the truth, which is a decision with
consequences in both directions. A grant that is live but unrecorded is taken
back out; a revocation that was applied but never recorded would be undone.
Where a failed command told you the record is the half that is behind, fix the
record first.

It also compares per dataset rather than per copy or per grant, because the
bucket and the key hold one pair of documents between them and every copy and
grant of that dataset is enforced by that same pair. That is why an unrecorded
grant on a dataset is not something reconciliation can repair "just for one
researcher": both documents are rendered whole from every grant the ledger
holds, and written over what is deployed.

## An s3 grant cannot be confirmed from the operator's side

Whether a researcher can actually read is not observable from this account. The
three ways it fails are indistinguishable to them and to you: a VPC identifier
that does not match theirs, an expiry that has passed, and a key that cannot
decrypt all present as the same access denial. `share s3` and a restoring
`extend s3` both say so on standard error, because saying it is the difference
between an operator checking and an operator assuming. The only confirmation is
a read of an object from inside the target VPC.

The two mechanisms also take the researcher's VPC in two different forms, and
the names are near-homographs: `share htsget --target-vpc-arn` wants the full
EC2 VPC ARN, because that is what VPC Lattice reports as the source of a
request, while `share s3 --target-vpc-id` wants the bare `vpc-…` identifier,
because that is what the bucket policy condition compares against. Passing one
where the other belongs writes successfully and denies everything.

## Destroying an s3 copy has exactly one window, and it is on the key

`purge s3` destroys this account's copy of a dataset by scheduling deletion of
the key it is encrypted under. Access stops at once — a key pending deletion
decrypts nothing — but the destruction itself is reversible for as long as KMS's
pending window runs. That window is 7 to 30 days, the CLI defaults to the
longest of them, and it is the **only** way back. Three things follow:

- **A purge is keyed on the dataset**, never on one copy or one researcher's
  access, because one key encrypts one dataset's bucket. Every copy of that
  dataset and every grant made against those copies dies together, and all of
  them are listed before the confirmation is asked for.
- **`restore s3` is what takes it back**, and it gives nobody their access
  again. The purge withdrew every grant *before* it scheduled the key — which is
  precisely what makes cancelling safe — and those withdrawals stand. A restore
  puts a readable copy back in this account's hands and nothing more; a fresh
  `share s3` is what gives it away again, and it costs a policy write rather
  than another transfer.
- **`delete s3` closes the window for good.** It empties the bucket, which buys
  storage and nothing else: access ended at the purge, and the bucket's own
  lifecycle rule would expire the ciphertext on its own in any case, a year after
  it was written unless the deployment set another `dataRetentionDays`. There is
  no window on it and nothing to cancel. It is not a second kill switch, and
  anyone reaching for it to stop somebody reading has the wrong command.

Nothing in any of this reads or writes the external source store. The
authoritative copy of the data lives there, untouched throughout, which is why a
purged — or even a deleted — dataset can be staged in again from scratch. What
a researcher has already downloaded is beyond the reach of this or any other
command.

## Copies are transferred once; grants are not transfers

A dataset is `stage`d once and granted as often as it needs to be. `stage s3`
refuses a dataset already covered by a copy that is `copying` or `staged`,
whether or not anyone holds a grant on it, because a second transfer would run
for hours and be billed to deliver what is already there. Two consequences worth
planning around:

- **A withdrawal is cheap to reverse.** `revoke s3` removes statements; sharing
  again mints a new grant and writes them back. Nothing is re-copied, and the
  new grant's identifier is a new one.
- **Re-copying is expensive and needs the dataset out of the way first.** The
  route back to a fresh copy is `purge s3` and then `stage s3` again — which is
  also the only route for a copy that `failed`, since a partial copy is worse
  than none: the bucket may hold some of the dataset, and it looks the same.

The copy itself runs for hours to days and reports its own outcome as an event,
so `stage s3` returns as soon as the transfer has been started. Nothing can be
granted until the copy lands. Where a dataset holds more objects than the copy
service will expand one wildcard to, the run is refused entirely rather than
partly done; stage it again with `--objects-from` naming the objects, which are
headed one at a time and carry no such limit.
