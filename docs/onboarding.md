# Researcher_Account Onboarding

This is the end-to-end procedure for onboarding a new Researcher_Account so it can
consume shared data via htsget-rs over VPC Lattice, and read its manifest from the
Data Control Manifest_Bucket. It ends in a `validate` run that confirms the setup
before the first real share (Req 18.6).

A researcher's access has two independent planes, and both must be in place:

1. **The data plane** — the researcher's VPC reaches htsget-rs through a VPC Lattice
   association backed by an accepted AWS RAM share.
2. **The manifest plane** — the researcher reads its own manifest object cross-account.
   This requires a grant on *both* sides: the Manifest_Bucket policy (Data Control side,
   owned by this project) and an IAM policy in the researcher account (researcher side).

Neither plane grants the other. The manifest is descriptive only and is never consulted
for authorization (`docs/manifest.md`, Req 7.8); access is enforced server-side from the
Share_Records alone.

Throughout, `<RESEARCHER_ACCOUNT_ID>` is the 12-digit AWS account ID being onboarded.

---

## Step 1 — Accept the AWS RAM resource share

The Data Control account shares the VPC Lattice service network to the researcher via
AWS RAM (owned elsewhere; see `docs/prerequisites.md`). The researcher account must
**accept** the share.

- Owner: the researcher's onboarding deployment (Researcher_Account side).
- In the researcher account:
  ```
  aws ram get-resource-share-invitations
  aws ram accept-resource-share-invitation --resource-share-invitation-arn <arn>
  ```
- Verify from the operator side later with `validate --account <RESEARCHER_ACCOUNT_ID>`,
  which checks the RAM resource share is accepted (Req 15.3).

## Step 2 — Associate the VPC Lattice service network

With the RAM share accepted, associate the researcher's VPC with the shared VPC Lattice
service network so requests can reach the htsget-rs endpoint.

- Owner: the researcher's onboarding deployment.
- Create a service-network / VPC association for the researcher VPC against the shared
  service network.
- Verify: `validate --account <RESEARCHER_ACCOUNT_ID>` confirms the association is
  present and in an active/associated state (Req 5, Req 15.3). Until it is active, the
  `share` command refuses to create Share_Records for the account and reports the
  connectivity gap (Req 5.4).

## Step 3 — Grant the researcher read on its manifest prefix (Data Control side)

Add the account to the Manifest_Bucket cross-account read policy. The set of granted
accounts is a **parameter of the Infrastructure_Stack**, so onboarding is a configuration
change, not a code change (Req 16.5).

- Owner: this project (Data Control side).
- Add `<RESEARCHER_ACCOUNT_ID>` to the stack's list of researcher accounts granted
  manifest read and redeploy the Infrastructure_Stack (see `docs/deployment.md`). The
  stack updates the bucket policy to grant that account `s3:GetObject` scoped to
  `manifests/<RESEARCHER_ACCOUNT_ID>/*` and nothing else. Where the bucket uses SSE-KMS,
  the key policy is updated with a matching per-account decrypt grant.
- Verify:
  ```
  aws s3api get-bucket-policy --bucket <MANIFEST_BUCKET>
  ```
  and confirm a statement grants the account read on its own prefix. `validate
  --account <RESEARCHER_ACCOUNT_ID>` also checks this grant and reports when the
  onboarding step that adds it is incomplete (Req 15.9).

> The manifest write itself is a same-account write performed by the Data Control
> identity; the Data_Releaser never creates or modifies the Manifest_Bucket or its policy
> (Req 7.9, 11.5).

## Step 4 — Grant the researcher's own principals read (Researcher side)

A cross-account read requires the identity side too. In the researcher account, attach an
IAM identity-based policy permitting the principals that will consume the manifest to
call `s3:GetObject` on
`arn:aws:s3:::<MANIFEST_BUCKET>/manifests/<RESEARCHER_ACCOUNT_ID>/*` (and, where the
bucket is SSE-KMS encrypted, `kms:Decrypt` on the key).

- Owner: the researcher's onboarding deployment (Researcher_Account side).
- Verify: from the researcher account, a real `s3:GetObject` against
  `manifests/<RESEARCHER_ACCOUNT_ID>/manifest.json` succeeds once a manifest exists.
  (`validate` checks only the resource-side grant in Step 3; the identity-side policy is
  verified within the researcher account.)

## Step 5 — (Optional) Deploy the Test_Lambda

The htsget VPC Lattice consumer construct can deploy an in-VPC `Test_Lambda` via its
`deployTestLambda` option. It calls htsget-rs across VPC Lattice from inside the
researcher's VPC and returns the response, enabling a genuine end-to-end check from the
consumer side.

- Owner: the researcher's onboarding deployment (via the consumer construct).
- The Test_Lambda's resource policy must permit the Data Control account to **list and
  invoke** it (Req 15.4).
- If it is not deployed, `validate` still runs and reports the provider-side and
  association checks, noting that the in-VPC end-to-end probe is unavailable and
  recommending `deployTestLambda` (Req 15.5).

## Step 6 — Confirm with `validate` before the first real share

Run the researcher-scoped validation from an operator session in the Data Control
account:

```
data-releaser validate --account <RESEARCHER_ACCOUNT_ID>
```

This performs the provider-side checks (credentials/permissions, OrcaVault Mart
reachability, Authorization_Service, htsget-rs health, cross-account data access) plus
the researcher-side checks for this account:

- VPC Lattice service network association is present and active, and the RAM share is
  accepted (Step 1–2, Req 15.3).
- The Manifest_Bucket policy grants this account read on its own prefix (Step 3,
  Req 15.9).
- If deployed, the Test_Lambda is discovered and invoked, and the check passes only if
  the invocation returns `statusCode` 200 with a valid htsget response under
  `latticeResponse` (Step 5, Req 15.4).

Each check reports pass/fail independently; `validate` continues past a failure and runs
the remaining checks rather than aborting on the first (Req 15.7), and exits non-zero if
any check failed. Onboarding is complete when the researcher-side checks pass. At that
point you can create the first share:

```
data-releaser share --libraries <ids> --account <RESEARCHER_ACCOUNT_ID> [--expiry 30d]
```

See `docs/usage.md` for the `share` command details and `docs/operations.md` for the
operational characteristics (revocation propagation, immutable library sets, idempotency,
and warehouse lag) an administrator should understand before sharing.
