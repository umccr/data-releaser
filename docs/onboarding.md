# Researcher account onboarding

This is the end-to-end procedure for onboarding a new researcher account so it can consume
shared data via htsget-rs over VPC Lattice, and read its manifest from the Data Control
manifest bucket. It ends in a `validate` run that confirms the setup before the first real
share.

A researcher's access has two independent planes, and both must be in place:

1. **The data plane** — the researcher's VPC reaches htsget-rs through a VPC Lattice
   association backed by an accepted AWS RAM share.
2. **The manifest plane** — the researcher reads its own manifest object cross-account.
   This requires a grant on *both* sides: the manifest bucket policy (Data Control side,
   owned by this project) and an IAM policy in the researcher account (researcher side).

Neither plane grants the other. The manifest is descriptive only and is never consulted
for authorization (`docs/manifest.md`); access is enforced server-side from the share
records alone.

Throughout, `<RESEARCHER_ACCOUNT_ID>` is the 12-digit AWS account ID being onboarded.

> This page covers onboarding for the **htsget** mechanism. The s3 mechanism has no RAM or
> VPC Lattice step — a researcher there reads a bucket in this account directly, from a VPC
> they nominate — but it shares the manifest plane, and step 3 grants both mechanisms'
> prefixes in one statement.

---

## Step 1 — Accept the AWS RAM resource share

The Data Control account shares the VPC Lattice service network to the researcher via
AWS RAM (owned elsewhere; see `docs/prerequisites.md`). The researcher account must
**accept** the share.

- Owner: the researcher's onboarding deployment (researcher side).
- In the researcher account:
  ```
  aws ram get-resource-share-invitations
  aws ram accept-resource-share-invitation --resource-share-invitation-arn <arn>
  ```
- Verify from the operator side later with
  `data-releaser validate htsget --account <RESEARCHER_ACCOUNT_ID>`, whose VPC Lattice /
  RAM association check queries AWS RAM for a principal association naming that account
  and passes only when one of them is in `ASSOCIATED` state. Where associations exist but
  none is associated, the check reports the statuses it found, so a pending invitation
  reads differently from a missing share.

## Step 2 — Associate the VPC Lattice service network

With the RAM share accepted, associate the researcher's VPC with the shared VPC Lattice
service network so requests can reach the htsget-rs endpoint.

- Owner: the researcher's onboarding deployment.
- Create a service-network / VPC association for the researcher VPC against the shared
  service network.
- Verify: the association itself is the researcher's to confirm in their own account. The
  operator-side check queries AWS RAM, so it sees the resource share reaching the account
  rather than the VPC association behind it — treat a passing check as "the share is in
  place", not "the VPC is attached".
- Until the RAM association reports `ASSOCIATED`, `share htsget` refuses to create a share
  record for the account and reports the connectivity gap. That preflight runs before
  anything is written, so a share is never persisted for an account htsget-rs cannot be
  reached from.

## Step 3 — Grant the researcher read on its manifest prefixes (Data Control side)

Add the account to the manifest bucket's cross-account read policy. The set of granted
accounts is a **parameter of the CDK stack** (`researcherAccountIds`), so onboarding is a
configuration change, not a code change.

- Owner: this project (Data Control side).
- Add `<RESEARCHER_ACCOUNT_ID>` to `researcherAccountIds` and redeploy the infrastructure
  stack (see `docs/deployment.md`). The stack writes one bucket-policy statement per
  account, granting `s3:GetObject` on `manifests/<RESEARCHER_ACCOUNT_ID>/*` and
  `datasets/<RESEARCHER_ACCOUNT_ID>/*`, and nothing else.
- Both prefixes are granted whichever mechanism the account uses today. Which kind of
  access it holds changes over the life of the account without this stack being
  redeployed, and granting only the prefix in use would leave a manifest that is written
  successfully and cannot be read — with nothing to report it, since the write is
  same-account and succeeds and the document is descriptive only. The researcher would
  keep working access and simply be unable to discover where the data is. Reading a prefix
  that holds no document is not a disclosure: the account is scoped to its own prefix under
  both, so the widest this reaches is an account learning it has no manifest of one kind,
  which it already knows.
- The manifest bucket is encrypted with S3-managed keys, so there is no key policy to
  extend alongside the bucket policy.
- Verify:
  ```
  aws s3api get-bucket-policy --bucket <MANIFEST_BUCKET>
  ```
  and confirm a statement grants the account read on its own prefix.
  `data-releaser validate htsget --account <RESEARCHER_ACCOUNT_ID>` also checks this grant
  and reports when the onboarding step that adds it is incomplete. Two limits are worth
  knowing: it looks for the htsget prefix only, and it looks for a *grant* rather than
  evaluating the policy — an explicit Deny elsewhere in the document would still override
  the Allow it matched, so a pass means the onboarding grant is present, not that the read
  is certain to succeed.

> The manifest write itself is a same-account write performed by the Data Control
> identity; the CLI never creates or modifies the manifest bucket or its policy.

## Step 4 — Grant the researcher's own principals read (researcher side)

A cross-account read requires the identity side too. In the researcher account, attach an
IAM identity-based policy permitting the principals that will consume the manifest to
call `s3:GetObject` on
`arn:aws:s3:::<MANIFEST_BUCKET>/manifests/<RESEARCHER_ACCOUNT_ID>/*` — and on
`.../datasets/<RESEARCHER_ACCOUNT_ID>/*` as well where the account also holds direct-read
dataset access.

- Owner: the researcher's onboarding deployment (researcher side).
- Verify: from the researcher account, a real `s3:GetObject` against
  `manifests/<RESEARCHER_ACCOUNT_ID>/manifest.json` succeeds once a manifest exists.
  (`validate` checks only the resource-side grant in Step 3; the identity-side policy can
  only be verified from within the researcher account.)

## Step 5 — (Optional) Deploy the test probe Lambda

The htsget VPC Lattice consumer construct can deploy an in-VPC test probe via its
`deployTestLambda` option. It calls htsget-rs across VPC Lattice from inside the
researcher's VPC and returns the response, which is the only genuine end-to-end check
available from the consumer side.

- Owner: the researcher's onboarding deployment (via the consumer construct).
- Discovery is by name: `validate` lists Lambda functions and takes the first whose name
  contains `TestLambda`, case-insensitively.
- The CLI assumes no role of its own, so the session running `validate` must already be
  able to call `lambda:ListFunctions` and `lambda:InvokeFunction` in the researcher
  account — through a role there named by `--profile`, for instance.
- If it is not deployed, `validate` still runs and reports the provider-side and
  association checks, noting that the in-VPC end-to-end probe is unavailable and
  recommending `deployTestLambda`. An absent probe is optional, so it is reported as
  unavailable-but-passing rather than as a failure.

## Step 6 — Confirm with `validate` before the first real share

Run the researcher-scoped validation from an operator session in the Data Control
account:

```
data-releaser validate htsget --account <RESEARCHER_ACCOUNT_ID>
```

Name the mechanism here. `validate` with no mechanism covers both — the right default for
a read-only question, since narrowing one by accident hides access rather than granting
it — but a bare run reports an unconfigured s3 mechanism as a **failed** check and exits
non-zero. On a deployment that releases only through htsget, that is a failure with
nothing to do with the account being onboarded. Naming `htsget` narrows the run to the
checks this procedure is about, and makes that mechanism's configuration a hard
requirement instead.

This performs the provider-side checks (credential resolution, OrcaVault Mart
reachability, the authorization service, htsget-rs health, cross-account data access, and
the deployed ledger table, manifest bucket and API Gateway stage) plus the researcher-side
checks for this account:

- The VPC Lattice / RAM association for the account is present and `ASSOCIATED`
  (Steps 1–2).
- The manifest bucket policy grants this account read on its own htsget prefix (Step 3).
- If deployed, the test probe is discovered and invoked, and the check passes only if the
  invocation returns `statusCode` 200 with a non-empty `latticeResponse` (Step 5).

Each check reports pass/fail independently; `validate` continues past a failure and runs
the remaining checks rather than aborting on the first — a deployment with three problems
should take one run to discover, not three — and it exits non-zero if any check failed.
Onboarding is complete when the researcher-side checks pass. At that point you can create
the first share:

```
data-releaser share htsget --libraries <ids> --account <RESEARCHER_ACCOUNT_ID> [--expiry 30d]
```

The mechanism is part of the invocation and is never inferred from the flags given, so
`data-releaser share --libraries ...` is refused rather than guessed at: the two mechanisms
grant quite different access, and choosing the wrong one surfaces only much later as an
unexplained deny or an unintended grant. An expiry is optional and must be at least a day;
without one the share is unlimited. The researcher's VPC ARN comes from
`researchers.<account>.targetVpcArn` in the config file unless `--target-vpc-arn`
supplies it.

See `docs/usage.md` for the `share` command details and `docs/operations.md` for the
operational characteristics (revocation propagation, immutable library sets, idempotency,
and warehouse lag) an administrator should understand before sharing.
