package dataset

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/micahhausler/aws-iam-policy/policy"
	"pgregory.net/rapid"

	"github.com/umccr/data-releaser/internal/awstest"
	"github.com/umccr/data-releaser/internal/testgen"
)

// This file holds the generative tests for the S3 release mechanism, built on
// pgregory.net/rapid with the shared generators in internal/testgen.
//
// The two rendered policy documents are the whole of the enforcement, so what matters
// about them is not that a particular example comes out right but that no set of grants
// the ledger can hold renders documents giving away more or less than those grants
// describe. That is an invariant over every copy of every dataset rather than over a
// handful of fixtures, which is why it is stated here.
//
// A copy carrying several grants is what these have to cover and a fixture cannot: the
// grants share both documents, so every one of them is a chance for one grant's statements
// to pick up another's account, VPC or expiry, and for a grant that was withdrawn to
// survive in the statements of one that was not.

// A rendered pair of documents gives away exactly the access the copy's deployed grants
// record: each researcher account, reachable only from the VPC its own grant names, until
// the expiry that grant records -- and for a grant that has been withdrawn, or a copy that
// has none, nothing at all.
//
// The pair is also checked for the two ways it could be wrong while reading plausibly. An
// account nobody expected appearing as a principal is access granted to someone the ledger
// never mentions. A Deny wide enough to catch the owning account -- one addressed to every
// principal and bounded by a VPC or an expiry -- takes away the account's access to its own
// bucket and key, and with it the ability to render the next document at all. Both surface
// hours later as an access denial indistinguishable from every other cause.
func TestRenderedPoliciesGrantExactlyWhatTheGrantsRecord(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := drawRenderInputs(t)

		policies, err := RenderPolicies(in.Target, in.Settings)
		if err != nil {
			t.Fatalf("rendering grants the ledger can hold was refused: %v", err)
		}

		checkGrantedAccess(t, in, policies)
		checkWithdrawnGrantsAreGone(t, in, policies)
		checkExpiryBound(t, in, policies)
		checkNamedAccounts(t, in, policies)
		checkDeniesCannotCatchTheOwner(t, in, policies)
		checkDeployedBaselineSurvives(t, in, policies)
		checkEveryStatementIsIdentifiedOnce(t, policies)
		checkTheSameGrantsRenderTheSameBytes(t, in)
	})
}

// checkGrantedAccess asserts every deployed grant gets the reads it describes and nothing
// else, and that the statements of one grant say nothing about another.
func checkGrantedAccess(t *rapid.T, in renderInputs, policies Policies) {
	t.Helper()

	bucketARN := fmt.Sprintf("arn:%s:s3:::%s", in.Partition, in.Target.HotCacheBucket)

	// Collected as the grants are checked, so that the statements naming a researcher can
	// be pinned to exactly the ones their own grant is entitled to.
	allowingInBucket := map[string][]string{}
	allowingInKey := map[string][]string{}

	for _, grant := range in.Deployed {
		root := accountRoot(in.Partition, grant.ResearcherAccountID)
		ids := statementIDsFor(grant)

		read, hasRead := statementByID(policies.Bucket, ids.read)
		if !hasRead {
			t.Fatalf("grant %s renders no read grant in the bucket policy", grant.GrantID)
		}
		decrypt, hasDecrypt := statementByID(policies.Key, ids.decrypt)
		if !hasDecrypt {
			t.Fatalf("grant %s renders no decrypt permission on the key, so every read fails", grant.GrantID)
		}

		assertSameSet(t, "grant principals", []string{root}, principalARNs(read))
		assertSameSet(t, "grant actions", []string{"s3:GetObject", "s3:ListBucket"}, actionsOf(read))
		assertSameSet(t, "grant resources", []string{bucketARN, bucketARN + "/*"}, resourcesOf(read))

		if got := singleConditionValue(t, read, "StringEquals", "aws:SourceVpc"); got != grant.TargetVpcID {
			t.Fatalf("grant %s admits VPC %q, the record names %q", grant.GrantID, got, grant.TargetVpcID)
		}
		// The VPC bound and the expiry are the whole of what qualifies a grant. Another
		// operator here narrows or widens access the ledger said nothing about.
		for operator := range read.Condition {
			if operator != "StringEquals" && operator != "DateLessThan" {
				t.Fatalf("grant %s carries an unexpected %q condition", grant.GrantID, operator)
			}
		}

		// The grant covers every object, including the copy's own artefacts, so those are
		// withheld by a Deny addressed to the same principal.
		controlDeny, found := statementByID(policies.Bucket, ids.denyControl)
		if !found {
			t.Fatalf("the copy's own artefacts are left readable by grant %s", grant.GrantID)
		}
		if controlDeny.Effect != policy.EffectDeny {
			t.Fatalf("the control-prefix statement of grant %s has effect %q", grant.GrantID, controlDeny.Effect)
		}
		assertSameSet(t, "control-prefix deny principals", []string{root}, principalARNs(controlDeny))
		assertSameSet(t, "control-prefix deny resources",
			[]string{bucketARN + "/" + ControlPrefix + "/*"}, resourcesOf(controlDeny))

		assertSameSet(t, "decrypt principals", []string{root}, principalARNs(decrypt))
		assertSameSet(t, "decrypt actions", []string{"kms:Decrypt", "kms:DescribeKey"}, actionsOf(decrypt))

		// Confining the decrypt permission to S3 in the bucket's own region is what makes
		// it useless to the researcher for anything but reading this bucket.
		viaService := singleConditionValue(t, decrypt, "StringEquals", "kms:ViaService")
		if !strings.HasPrefix(viaService, "s3."+in.Settings.Region+".") {
			t.Fatalf("grant %s conditions decrypt on %q, which does not name S3 in %q",
				grant.GrantID, viaService, in.Settings.Region)
		}

		allowingInBucket[root] = append(allowingInBucket[root], ids.read)
		allowingInKey[root] = append(allowingInKey[root], ids.decrypt)
	}

	// The key is never asked which VPC a request came from. S3 calls it from its own
	// infrastructure on the requester's behalf, so a VPC condition here denies reads that
	// did originate in the permitted VPC.
	for _, st := range allStatements(policies.Key) {
		if operands := vpcOperands(st); len(operands) > 0 {
			t.Fatalf("key policy statement %q conditions on %v", st.Sid, operands)
		}
	}

	// Those two statements are the only places each researcher is allowed anything. An
	// Allow naming one of them anywhere else is access the ledger does not describe, and
	// with several grants in one document the likeliest source of it is another grant's
	// statement.
	for root, sids := range allowingInBucket {
		assertSameSet(t, "bucket statements allowing "+root, sids, allowSidsNaming(policies.Bucket, root))
	}
	for root, sids := range allowingInKey {
		assertSameSet(t, "key statements allowing "+root, sids, allowSidsNaming(policies.Key, root))
	}
}

// checkWithdrawnGrantsAreGone asserts a grant that has been revoked leaves no trace in
// either document.
//
// The record still remembers who it was granted to and where they read from. That must
// stay in the ledger and out of the documents: a statement still comparing a request
// against that VPC is a document that still has an opinion about that researcher.
func checkWithdrawnGrantsAreGone(t *rapid.T, in renderInputs, policies Policies) {
	t.Helper()

	for _, grant := range in.Withdrawn {
		ids := statementIDsFor(grant)
		for _, sid := range []string{ids.read, ids.denyControl} {
			if _, found := statementByID(policies.Bucket, sid); found {
				t.Fatalf("withdrawn grant %s still has bucket policy statement %q", grant.GrantID, sid)
			}
		}
		if _, found := statementByID(policies.Key, ids.decrypt); found {
			t.Fatalf("withdrawn grant %s still has a decrypt permission on the key", grant.GrantID)
		}

		root := accountRoot(in.Partition, grant.ResearcherAccountID)
		for _, st := range bothDocuments(policies) {
			if slices.Contains(principalARNs(st), root) {
				t.Fatalf("statement %q names the researcher of withdrawn grant %s", st.Sid, grant.GrantID)
			}
			for operator, operands := range st.Condition {
				for key, value := range operands {
					if slices.Contains(conditionStrings(value), grant.TargetVpcID) {
						t.Fatalf("statement %q is conditioned on the VPC of withdrawn grant %s (%s/%s)",
							st.Sid, grant.GrantID, operator, key)
					}
				}
			}
		}
	}
}

// checkExpiryBound asserts each deployed grant's recorded expiry lands on exactly that
// grant's two granting statements, and that an unlimited grant carries no time bound at
// all rather than a distant date.
func checkExpiryBound(t *rapid.T, in renderInputs, policies Policies) {
	t.Helper()

	// What the expiries ought to be, by the statement carrying them. Building it from the
	// grants and comparing the whole map is what catches an expiry landing on a statement
	// belonging to another grant, which is the way this goes wrong once a copy carries
	// more than one.
	expected := map[string]string{}
	for _, grant := range in.Deployed {
		if grant.IsUnlimited() {
			continue
		}
		ids := statementIDsFor(grant)
		expected[ids.read] = grant.ExpiresAt.UTC().Format(time.RFC3339)
		expected[ids.decrypt] = grant.ExpiresAt.UTC().Format(time.RFC3339)
	}

	bound := map[string]string{}
	for _, st := range bothDocuments(policies) {
		if !hasTimeCondition(st) {
			continue
		}
		for operator := range st.Condition {
			if strings.HasPrefix(operator, "Date") && operator != "DateLessThan" {
				t.Fatalf("statement %q bounds access with %q, which does not stop admitting requests at the expiry",
					st.Sid, operator)
			}
		}
		raw := singleConditionValue(t, st, "DateLessThan", "aws:CurrentTime")
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatalf("statement %q renders the expiry as %q, which is not a timestamp a condition compares: %v",
				st.Sid, raw, err)
		}
		bound[st.Sid] = at.UTC().Format(time.RFC3339)
	}

	assertSameSet(t, "statements carrying an expiry", keysOf(expected), keysOf(bound))
	for sid, want := range expected {
		if got := bound[sid]; got != want {
			t.Fatalf("statement %q stops admitting requests at %s, its grant records %s", sid, got, want)
		}
	}
}

// checkNamedAccounts asserts no account beyond those the deployed grants and the
// deployment settings mention is named as a principal, and that every principal is
// rendered in the bucket's own partition.
func checkNamedAccounts(t *rapid.T, in renderInputs, policies Policies) {
	t.Helper()

	for _, st := range bothDocuments(policies) {
		for _, arn := range principalARNs(st) {
			parsed, err := awsarn.Parse(arn)
			if err != nil {
				t.Fatalf("statement %q names principal %q, which is not an ARN and so names nobody", st.Sid, arn)
			}
			if parsed.Partition != in.Partition {
				t.Fatalf("statement %q names principal %q in partition %q rather than %q",
					st.Sid, arn, parsed.Partition, in.Partition)
			}
			if parsed.AccountID == "" {
				continue
			}
			if !in.PermittedAccounts[parsed.AccountID] {
				t.Fatalf("statement %q names account %s, which neither a deployed grant nor the deployment settings mention",
					st.Sid, parsed.AccountID)
			}
		}
	}
}

// checkDeniesCannotCatchTheOwner asserts no Deny can take the owning account's access to
// its own bucket and key away.
//
// One Deny may legitimately be addressed to every principal: the transport one, which
// refuses requests that did not arrive over TLS. Its condition is about how a request
// travelled rather than who made it or when, so it costs the owning account nothing, and
// scoping it to one researcher would leave the bucket accepting plaintext HTTP from
// everyone else. Bounding it by a VPC or an expiry instead would read as a tighter version
// of the same idea and would in fact lock the owning account out, which is why those bounds
// may only appear on statements naming one granted researcher.
func checkDeniesCannotCatchTheOwner(t *rapid.T, in renderInputs, policies Policies) {
	t.Helper()

	granted := make([]string, 0, len(in.Deployed))
	for _, grant := range in.Deployed {
		granted = append(granted, accountRoot(in.Partition, grant.ResearcherAccountID))
	}

	globalDenies := 0
	for _, st := range bothDocuments(policies) {
		if st.Effect != policy.EffectDeny {
			continue
		}
		boundedByAccess := hasTimeCondition(st) || len(vpcOperands(st)) > 0

		if namesEveryPrincipal(st.Principal) {
			globalDenies++
			if st.Sid != SidDenyInsecureTransport {
				t.Fatalf("statement %q denies every principal, including the owning account", st.Sid)
			}
			if boundedByAccess {
				t.Fatalf("statement %q denies every principal and is bounded by a VPC or an expiry, "+
					"which locks the owning account out of its own bucket", st.Sid)
			}
			continue
		}
		if !boundedByAccess {
			continue
		}
		named := principalARNs(st)
		if len(named) != 1 || !slices.Contains(granted, named[0]) {
			t.Fatalf("deny %q is bounded by a VPC or an expiry but names %v, which is not one granted researcher",
				st.Sid, named)
		}
	}

	if globalDenies != 1 {
		t.Fatalf("expected exactly one deny addressed to every principal (the transport one), got %d", globalDenies)
	}
}

// checkDeployedBaselineSurvives asserts a render never removes the statements the
// per-dataset deployment establishes.
//
// A rendered document replaces the deployed one whole, and withdrawing is this same render
// with a grant left out. So a statement missing here is a statement removed: without the
// account's key administration no principal can write the next key policy, and without the
// transport deny the bucket starts accepting plaintext HTTP.
func checkDeployedBaselineSurvives(t *rapid.T, in renderInputs, policies Policies) {
	t.Helper()

	ownerRoot := accountRoot(in.Partition, in.Settings.AccountID)

	if _, found := statementByID(policies.Bucket, SidDenyInsecureTransport); !found {
		t.Fatalf("a bucket policy was rendered that accepts plaintext HTTP")
	}
	admin, found := statementByID(policies.Key, SidAccountKeyAdministration)
	if !found {
		t.Fatalf("a key policy was rendered that no principal is permitted to rewrite")
	}
	assertSameSet(t, "key administration principals", []string{ownerRoot}, principalARNs(admin))
	if _, found := statementByID(policies.Key, SidCopyWriterEncrypt); !found {
		t.Fatalf("a key policy was rendered that the copy cannot write through")
	}
}

// checkEveryStatementIsIdentifiedOnce asserts each document identifies its statements
// uniquely. Reconciliation and repair both match statements by identifier, so two
// statements sharing one can be matched to neither, and an unnamed statement to nothing at
// all. Several grants in one document is where this stops being free.
func checkEveryStatementIsIdentifiedOnce(t *rapid.T, policies Policies) {
	t.Helper()

	for _, doc := range []*policy.Policy{policies.Bucket, policies.Key} {
		seen := map[string]bool{}
		for _, st := range allStatements(doc) {
			if st.Sid == "" {
				t.Fatalf("a statement was rendered without an identifier")
			}
			if seen[st.Sid] {
				t.Fatalf("statement identifier %q was rendered twice in one document", st.Sid)
			}
			seen[st.Sid] = true
		}
	}
}

// checkTheSameGrantsRenderTheSameBytes asserts the order the grants arrive in cannot be
// read back out of the documents.
//
// The grants of a copy are a set, and a query returns a set in whatever order it likes.
// Documents that reordered with it would differ from the deployed pair every time they
// were re-rendered, and reconciliation has no way to tell that difference from a grant
// somebody moved by hand.
func checkTheSameGrantsRenderTheSameBytes(t *rapid.T, in renderInputs) {
	t.Helper()

	shuffled := in.Target
	shuffled.Grants = slices.Clone(in.Target.Grants)
	slices.Reverse(shuffled.Grants)

	first, second := renderPair(t, in.Target, in.Settings), renderPair(t, shuffled, in.Settings)
	if first.bucket != second.bucket {
		t.Fatalf("the grants' order changed the bucket policy:\n%s\n%s", first.bucket, second.bucket)
	}
	if first.key != second.key {
		t.Fatalf("the grants' order changed the key policy:\n%s\n%s", first.key, second.key)
	}
}

// Applying a copy's grants brings the deployed documents to the access they record, from
// wherever they started, and applying them a second time changes nothing.
//
// Both halves carry weight. Re-applying is the repair for a document someone edited in
// the console, so it has to arrive at the same place from a grant nobody recorded, from
// a bucket carrying no policy at all, and from documents that are already right. And
// because it is the repair, it has to be safe to run against a dataset with nothing
// wrong with it: an application that wrote the documents differently the second time
// would have every later comparison reporting drift of its own making.
//
// The comparison runs against documents that have been written as JSON and read back,
// which is what makes the result mean anything. AWS does not hand a document back the way
// it was given, and a comparison that only ever saw the renderer's own values would say
// nothing about what is deployed.
func TestApplyingGrantsConvergesOnTheAccessTheyRecord(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ctx := context.Background()

		in := drawRenderInputs(t)
		start := drawStartingDeployment(t, in)
		deployed := start.deployed

		applier := NewPolicyApplier(in.Settings, deployed.objects, deployed.keys)
		differ := NewPolicyDiffer(in.Settings, deployed.objects, deployed.keys)

		// What was deployed to begin with is what the application has to correct, so the
		// comparison has to see it as needing correcting. Otherwise a differ that found
		// nothing anywhere would satisfy everything below.
		before, err := differ.Diff(ctx, in.Target)
		if err != nil {
			t.Fatalf("comparing what is deployed before anything is applied: %v", err)
		}
		if before.Matches() != start.matched {
			t.Fatalf("starting from %s, the comparison reports matching=%v before anything is applied:\n%s",
				start.whence, before.Matches(), before.Report())
		}

		first, err := applier.Apply(ctx, in.Target)
		if err != nil {
			t.Fatalf("applying grants the ledger can hold was refused: %v", err)
		}
		if deployed.bucketDocument != first.BucketDocument {
			t.Fatalf("the bucket policy now deployed is not the document the application reports writing")
		}
		if deployed.keyDocument != first.KeyDocument {
			t.Fatalf("the key policy now deployed is not the document the application reports writing")
		}

		converged, err := differ.Diff(ctx, in.Target)
		if err != nil {
			t.Fatalf("comparing the documents just applied: %v", err)
		}
		if !converged.Matches() {
			t.Fatalf("starting from %s, the documents just applied do not describe the access the ledger does:\n%s",
				start.whence, converged.Report())
		}

		second, err := applier.Apply(ctx, in.Target)
		if err != nil {
			t.Fatalf("applying the same grants again was refused: %v", err)
		}
		if !slices.Equal(first.Written, second.Written) {
			t.Fatalf("applying again wrote %v, the first application wrote %v", second.Written, first.Written)
		}
		if second.BucketDocument != first.BucketDocument {
			t.Fatalf("applying again rendered a different bucket policy:\nfirst:\n%s\nsecond:\n%s",
				first.BucketDocument, second.BucketDocument)
		}
		if second.KeyDocument != first.KeyDocument {
			t.Fatalf("applying again rendered a different key policy:\nfirst:\n%s\nsecond:\n%s",
				first.KeyDocument, second.KeyDocument)
		}
		checkRepeatedWritesAreIdentical(t, deployed)

		again, err := differ.Diff(ctx, in.Target)
		if err != nil {
			t.Fatalf("comparing the documents after a second application: %v", err)
		}
		if !again.Matches() {
			t.Fatalf("a second application moved the deployed access away from the ledger:\n%s", again.Report())
		}
	})
}

// checkRepeatedWritesAreIdentical asserts that every put of a document was the same
// document. Comparing the calls rather than the state they left behind is what catches
// an application that arrives at the right answer by writing a different one first.
func checkRepeatedWritesAreIdentical(t *rapid.T, deployed *deployedPair) {
	t.Helper()

	for _, document := range []struct {
		target ApplyTarget
		puts   []string
	}{
		{ApplyTargetBucket, deployed.bucketPuts()},
		{ApplyTargetKey, deployed.keyPuts()},
	} {
		for i, put := range document.puts {
			if put != document.puts[0] {
				t.Fatalf("write %d of the %s differs from the first:\nfirst:\n%s\nlater:\n%s",
					i+1, document.target, document.puts[0], put)
			}
		}
	}
}

// deployedPair stands in for the two documents AWS holds for one dataset: what is put on
// the bucket and on the key is what is read back from them.
//
// The round trip is the whole point. A rendered document is compared against one that
// has been written out and parsed again, which is the only version of the comparison that
// says anything about deployed access rather than about the renderer agreeing with
// itself.
type deployedPair struct {
	objects *awstest.S3
	keys    *awstest.KMS

	// bucketDocument and keyDocument are the documents currently in force. An empty
	// string stands for a document that is not deployed at all.
	bucketDocument string
	keyDocument    string
}

// newDeployedPair returns a stand-in holding the given documents, either of which may be
// empty for a document that is not deployed, with the key reporting the given state.
func newDeployedPair(keyState kmstypes.KeyState, bucketDocument, keyDocument string) *deployedPair {
	pair := &deployedPair{bucketDocument: bucketDocument, keyDocument: keyDocument}

	pair.objects = &awstest.S3{
		GetBucketPolicyFn: func(context.Context, *s3.GetBucketPolicyInput) (*s3.GetBucketPolicyOutput, error) {
			if pair.bucketDocument == "" {
				return nil, absentBucketPolicyError{}
			}
			return &s3.GetBucketPolicyOutput{Policy: aws.String(pair.bucketDocument)}, nil
		},
		PutBucketPolicyFn: func(_ context.Context, params *s3.PutBucketPolicyInput) (*s3.PutBucketPolicyOutput, error) {
			pair.bucketDocument = aws.ToString(params.Policy)
			return &s3.PutBucketPolicyOutput{}, nil
		},
	}
	pair.keys = &awstest.KMS{
		DescribeKeyFn: func(_ context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
			return &kms.DescribeKeyOutput{KeyMetadata: &kmstypes.KeyMetadata{
				KeyId:    params.KeyId,
				Enabled:  keyState == kmstypes.KeyStateEnabled,
				KeyState: keyState,
			}}, nil
		},
		GetKeyPolicyFn: func(context.Context, *kms.GetKeyPolicyInput) (*kms.GetKeyPolicyOutput, error) {
			// KMS keeps a policy for every key, so an empty answer stands for a key this
			// test has not written one to yet.
			if pair.keyDocument == "" {
				return &kms.GetKeyPolicyOutput{}, nil
			}
			return &kms.GetKeyPolicyOutput{Policy: aws.String(pair.keyDocument)}, nil
		},
		PutKeyPolicyFn: func(_ context.Context, params *kms.PutKeyPolicyInput) (*kms.PutKeyPolicyOutput, error) {
			pair.keyDocument = aws.ToString(params.Policy)
			return &kms.PutKeyPolicyOutput{}, nil
		},
	}
	return pair
}

// bucketPuts and keyPuts list every document that was written, in order.
func (p *deployedPair) bucketPuts() []string {
	puts := make([]string, 0, len(p.objects.PutBucketPolicyCalls))
	for _, call := range p.objects.PutBucketPolicyCalls {
		puts = append(puts, aws.ToString(call.Policy))
	}
	return puts
}

func (p *deployedPair) keyPuts() []string {
	puts := make([]string, 0, len(p.keys.PutKeyPolicyCalls))
	for _, call := range p.keys.PutKeyPolicyCalls {
		puts = append(puts, aws.ToString(call.Policy))
	}
	return puts
}

// convergenceStart is a deployed state a copy's grants are applied onto. Reconciliation
// has to reach the same place from any of them.
type convergenceStart struct {
	deployed *deployedPair

	// matched reports whether what is deployed already describes the access the ledger
	// does, which is the one starting state an application has nothing to correct from.
	matched bool

	// whence names the starting state, so a failure says which one it was.
	whence string
}

// drawStartingDeployment generates the deployed state the grants are applied onto: nothing
// at all, a grant the ledger has never described, or the documents it already describes.
//
// The second is drift as it actually occurs -- a grant edited in by hand, or one left
// behind by a grant since withdrawn. The third is the case re-applying has to be harmless
// in, since that is what a routine reconciliation does.
func drawStartingDeployment(t *rapid.T, in renderInputs) convergenceStart {
	keyState := kmstypes.KeyStateEnabled
	if !in.Granting && rapid.Bool().Draw(t, "keyDisabled") {
		// Withdrawing through a switched-off key still writes both documents: the key
		// being off is no reason to leave a grant standing. A key whose deletion is
		// scheduled is deliberately not drawn -- it accepts no further policy, so a grant
		// it still carries is destroyed with the key rather than corrected, and there is
		// nothing for the comparison to converge onto.
		keyState = kmstypes.KeyStateDisabled
	}

	switch rapid.SampledFrom([]string{"nothing", "aGrantNobodyRecorded", "whatTheLedgerDescribes"}).
		Draw(t, "deployedBefore") {
	case "whatTheLedgerDescribes":
		pair := renderPair(t, in.Target, in.Settings)
		return convergenceStart{
			deployed: newDeployedPair(keyState, pair.bucket, pair.key),
			matched:  true,
			whence:   "the documents the ledger already describes",
		}

	case "aGrantNobodyRecorded":
		// The same copy granted to an account the ledger has never named, alongside
		// whatever it does describe. That is what both a hand-edited grant and a grant
		// outliving its withdrawal look like from here.
		stranger := in.Target
		stranger.Grants = append(slices.Clone(in.Target.Grants), strangerGrant(t, in))

		pair := renderPair(t, stranger, in.Settings)
		return convergenceStart{
			deployed: newDeployedPair(keyState, pair.bucket, pair.key),
			matched:  false,
			whence:   "a grant to an account the ledger has never named",
		}

	default:
		return convergenceStart{
			deployed: newDeployedPair(keyState, "", ""),
			matched:  false,
			whence:   "no policy deployed at all",
		}
	}
}

// strangerGrant is a grant to an account the ledger does not describe, deployed against
// the same copy.
//
// Its id begins where no generated one does, so the statements it renders cannot collide
// with the statements of a grant the ledger does hold -- a collision would be a document
// the comparison could read as matching for the wrong reason.
func strangerGrant(t *rapid.T, in renderInputs) *Grant {
	expiry := time.Now().UTC().Truncate(time.Second).Add(hours(t, "strangerExpiresInHours", 1, 8760))
	return &Grant{
		GrantID:             "0Z" + testgen.ULID(t)[2:],
		DatasetURI:          in.Target.DatasetURI,
		HotCacheBucket:      in.Target.HotCacheBucket,
		KeyArn:              in.Target.KeyArn,
		ResearcherAccountID: drawUnrelatedAccount(t, in.PermittedAccounts),
		TargetVpcID:         "vpc-" + rapid.StringMatching(`[0-9a-f]{17}`).Draw(t, "strangerVpcId"),
		State:               GrantStateGranted,
		CreatedAt:           time.Now().UTC().Truncate(time.Second),
		ExpiresAt:           &expiry,
	}
}

// documentPair is the two documents as an applier would write them.
type documentPair struct {
	bucket string
	key    string
}

// renderPair renders a target's two documents as the applier would write them.
func renderPair(t *rapid.T, target PolicyTarget, settings PolicySettings) documentPair {
	t.Helper()

	policies, err := RenderPolicies(target, settings)
	if err != nil {
		t.Fatalf("rendering grants the ledger can hold was refused: %v", err)
	}
	bucket, err := policies.BucketPolicyJSON()
	if err != nil {
		t.Fatalf("rendering the bucket policy as the document S3 accepts: %v", err)
	}
	key, err := policies.KeyPolicyJSON()
	if err != nil {
		t.Fatalf("rendering the key policy as the document KMS accepts: %v", err)
	}
	return documentPair{bucket: bucket, key: key}
}

// drawUnrelatedAccount draws an account id none of the known ones, so a grant to it is
// unambiguously a grant the ledger does not describe.
func drawUnrelatedAccount(t *rapid.T, known map[string]bool) string {
	for {
		if candidate := testgen.AccountID(t); !known[candidate] {
			return candidate
		}
	}
}

// renderInputs is a copy the ledger could hold, with the grants made against it and the
// deployment settings its documents are rendered with, plus the facts needed to state
// expectations about the result without deriving them the way the renderer does.
type renderInputs struct {
	Target   PolicyTarget
	Settings PolicySettings

	// Partition every ARN in both documents must be rendered in.
	Partition string

	// Deployed are the grants whose statements the documents must carry, and Withdrawn
	// those that must appear in neither. Expiry does not enter into it: an expired grant
	// keeps its statements, and the condition inside them is what turns requests away.
	Deployed  []*Grant
	Withdrawn []*Grant

	// Granting is whether anything at all is granted, which is what decides whether the
	// key has to be able to serve reads.
	Granting bool

	// PermittedAccounts is every account the documents may name as a principal.
	PermittedAccounts map[string]bool
}

// drawRenderInputs generates a copy carrying any number of grants the ledger can hold --
// none at all, one, or several -- mixing unlimited with expiring and deployed with
// withdrawn, against settings for a deployment in any partition.
//
// Everything drawn is valid, because rendering refuses input it cannot express and a
// refusal would say nothing about what a document grants. The refusals have their own
// tests.
func drawRenderInputs(t *rapid.T) renderInputs {
	partition := rapid.SampledFrom([]string{"aws", "aws-cn", "aws-us-gov"}).Draw(t, "partition")
	region := rapid.SampledFrom([]string{"ap-southeast-2", "us-east-1", "eu-west-1", "cn-north-1"}).Draw(t, "region")

	// Distinct accounts throughout, so an account turning up in a document it has no
	// business in is a failure rather than a coincidence.
	accounts := drawDistinctAccounts(t, 3)
	owner, copyWriterAccount, custodianAccount := accounts[0], accounts[1], accounts[2]

	settings := PolicySettings{
		AccountID:          owner,
		Region:             region,
		CopyWriterRoleName: drawRoleName(t, "copyWriterRole"),
	}
	permitted := map[string]bool{owner: true}
	spokenFor := map[string]bool{owner: true, copyWriterAccount: true, custodianAccount: true}

	// The copy orchestration may be installed alongside the cache bucket, in which case
	// the settings leave its account to default to the owning one.
	if rapid.Bool().Draw(t, "copyWriterInAnotherAccount") {
		settings.CopyWriterRoleAccountID = copyWriterAccount
		permitted[copyWriterAccount] = true
	}

	// A separate custodian may not exist, in which case withdrawal stays reachable
	// through the account's own key administration and no statement is written.
	if rapid.Bool().Draw(t, "hasCustodian") {
		settings.CustodianRoleARN = fmt.Sprintf("arn:%s:iam::%s:role/%s",
			partition, custodianAccount, drawRoleName(t, "custodianRole"))
		permitted[custodianAccount] = true
	}

	target := PolicyTarget{
		DatasetURI:     "s3://ceph-source/datasets/" + rapid.StringMatching(`[a-z0-9-]{1,12}`).Draw(t, "dataset") + "/",
		HotCacheBucket: rapid.StringMatching(`[a-z0-9][a-z0-9-]{1,40}[a-z0-9]`).Draw(t, "hotCacheBucket"),
		KeyArn: fmt.Sprintf("arn:%s:kms:%s:%s:key/%s", partition, region, owner,
			rapid.StringMatching(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`).Draw(t, "keyId")),
	}

	// The partition is read from the key ARN when the settings do not state one; both
	// paths must reach the same answer.
	if rapid.Bool().Draw(t, "partitionStated") {
		settings.Partition = partition
	}

	in := renderInputs{
		Settings:          settings,
		Partition:         partition,
		PermittedAccounts: permitted,
	}

	copyID := testgen.ULID(t)
	seenGrants := map[string]bool{}
	seenVpcs := map[string]bool{}
	for range rapid.IntRange(0, 3).Draw(t, "grantCount") {
		grant := drawGrant(t, target, copyID, spokenFor, seenGrants, seenVpcs)
		target.Grants = append(target.Grants, grant)

		if grant.IsDeployed() {
			in.Deployed = append(in.Deployed, grant)
			permitted[grant.ResearcherAccountID] = true
			continue
		}
		in.Withdrawn = append(in.Withdrawn, grant)
	}

	in.Target = target
	in.Granting = len(in.Deployed) > 0
	return in
}

// drawGrant generates one grant against the copy: its own researcher, its own VPC, its own
// expiry, either still deployed or withdrawn.
//
// Every grant gets an account and a VPC no other grant has, so a value appearing in a
// statement it does not belong to is a failure rather than a coincidence -- which is the
// whole point of generating more than one of them.
func drawGrant(t *rapid.T, target PolicyTarget, copyID string, spokenFor, seenGrants, seenVpcs map[string]bool) *Grant {
	now := time.Now().UTC().Truncate(time.Second)

	grantID := testgen.ULID(t)
	for seenGrants[grantID] {
		grantID = testgen.ULID(t)
	}
	seenGrants[grantID] = true

	researcher := drawUnrelatedAccount(t, spokenFor)
	spokenFor[researcher] = true

	vpc := "vpc-" + rapid.StringMatching(`[0-9a-f]{17}`).Draw(t, "vpcId")
	for seenVpcs[vpc] {
		vpc = "vpc-" + rapid.StringMatching(`[0-9a-f]{17}`).Draw(t, "vpcId")
	}
	seenVpcs[vpc] = true

	grant := &Grant{
		GrantID:             grantID,
		CopyID:              copyID,
		DatasetURI:          target.DatasetURI,
		HotCacheBucket:      target.HotCacheBucket,
		KeyArn:              target.KeyArn,
		ResearcherAccountID: researcher,
		TargetVpcID:         vpc,
		State:               rapid.SampledFrom(GrantStates()).Draw(t, "grantState"),
		CreatedAt:           now.Add(-hours(t, "createdHoursAgo", 1, 720)),
	}

	// Whole seconds, because that is the precision a policy condition compares at. An
	// expiry already in the past is drawn deliberately: it does not withdraw the
	// statements, it makes them stop admitting requests.
	switch rapid.SampledFrom([]string{"unlimited", "future", "past"}).Draw(t, "expiry") {
	case "future":
		at := now.Add(hours(t, "expiresInHours", 1, 8760))
		grant.ExpiresAt = &at
	case "past":
		at := now.Add(-hours(t, "expiredHoursAgo", 1, 720))
		grant.ExpiresAt = &at
	}

	// A withdrawn grant keeps everything it recorded; the withdrawal is what stops it
	// being written, and it is part of the record of what happened.
	if grant.State == GrantStateRevoked {
		at := now.Add(-hours(t, "revokedHoursAgo", 1, 72))
		grant.RevokedAt = &at
	}
	return grant
}

// drawDistinctAccounts draws n distinct account ids.
func drawDistinctAccounts(t *rapid.T, n int) []string {
	accounts := make([]string, 0, n)
	seen := make(map[string]bool, n)
	for len(accounts) < n {
		candidate := testgen.AccountID(t)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		accounts = append(accounts, candidate)
	}
	return accounts
}

func drawRoleName(t *rapid.T, label string) string {
	return rapid.StringMatching(`[a-z][a-z0-9-]{2,20}`).Draw(t, label)
}

func hours(t *rapid.T, label string, low, high int64) time.Duration {
	return time.Duration(rapid.Int64Range(low, high).Draw(t, label)) * time.Hour
}

// accountRoot names an account as the documents name it.
func accountRoot(partition, accountID string) string {
	return fmt.Sprintf("arn:%s:iam::%s:root", partition, accountID)
}

// bothDocuments returns every statement of the pair, for the invariants that hold of
// each document equally.
func bothDocuments(policies Policies) []policy.Statement {
	bucket := allStatements(policies.Bucket)
	key := allStatements(policies.Key)

	all := make([]policy.Statement, 0, len(bucket)+len(key))
	all = append(all, bucket...)
	all = append(all, key...)
	return all
}

func allStatements(doc *policy.Policy) []policy.Statement {
	if doc == nil || doc.Statements == nil {
		return nil
	}
	return doc.Statements.Values()
}

// statementByID finds a statement by its identifier, which is how a statement is matched
// through the reordering marshalling and the console are free to do.
func statementByID(doc *policy.Policy, sid string) (policy.Statement, bool) {
	for _, st := range allStatements(doc) {
		if st.Sid == sid {
			return st, true
		}
	}
	return policy.Statement{}, false
}

// principalARNs lists the AWS principals a statement names. A statement addressed to
// every principal names none, so it yields nothing here.
func principalARNs(st policy.Statement) []string {
	if st.Principal == nil {
		return nil
	}
	awsPrincipal := st.Principal.AWS()
	if awsPrincipal == nil {
		return nil
	}
	return awsPrincipal.Values()
}

// namesEveryPrincipal reports whether a statement is addressed to everyone.
func namesEveryPrincipal(p *policy.Principal) bool {
	if p == nil {
		return false
	}
	return slices.Contains(p.Kinds(), policy.PrincipalKindAll)
}

// allowSidsNaming lists the Allow statements in doc that name the given principal.
func allowSidsNaming(doc *policy.Policy, principalARN string) []string {
	var sids []string
	for _, st := range allStatements(doc) {
		if st.Effect != policy.EffectAllow {
			continue
		}
		if slices.Contains(principalARNs(st), principalARN) {
			sids = append(sids, st.Sid)
		}
	}
	return sids
}

func actionsOf(st policy.Statement) []string {
	if st.Action == nil {
		return nil
	}
	return st.Action.Values()
}

func resourcesOf(st policy.Statement) []string {
	if st.Resource == nil {
		return nil
	}
	return st.Resource.Values()
}

func conditionStrings(value *policy.ConditionValue) []string {
	if value == nil {
		return nil
	}
	strs, _, _ := value.Values()
	return strs
}

func singleConditionValue(t *rapid.T, st policy.Statement, operator, key string) string {
	t.Helper()

	operands, found := st.Condition[operator]
	if !found {
		t.Fatalf("statement %q carries no %s condition", st.Sid, operator)
	}
	value, found := operands[key]
	if !found {
		t.Fatalf("statement %q carries no %s condition on %s", st.Sid, operator, key)
	}
	values := conditionStrings(value)
	if len(values) != 1 {
		t.Fatalf("statement %q condition %s/%s holds %v, expected a single value", st.Sid, operator, key, values)
	}
	return values[0]
}

// hasTimeCondition reports whether a statement is bounded by a date, which is how an
// expiry is expressed.
func hasTimeCondition(st policy.Statement) bool {
	for operator := range st.Condition {
		if strings.HasPrefix(operator, "Date") {
			return true
		}
	}
	return false
}

// vpcOperands lists the condition keys a statement compares a request's VPC against.
func vpcOperands(st policy.Statement) []string {
	var keys []string
	for _, operands := range st.Condition {
		for key := range operands {
			if strings.Contains(strings.ToLower(key), "vpc") {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// assertSameSet fails unless got holds exactly the values in want. Order carries no
// meaning in a policy document, so it is ignored; multiplicity is not, so duplicates
// still fail.
func assertSameSet(t *rapid.T, what string, want, got []string) {
	t.Helper()

	wantSorted := slices.Sorted(slices.Values(want))
	gotSorted := slices.Sorted(slices.Values(got))
	if !slices.Equal(wantSorted, gotSorted) {
		t.Fatalf("%s: expected %v, got %v", what, wantSorted, gotSorted)
	}
}
