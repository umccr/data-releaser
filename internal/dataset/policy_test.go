package dataset

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/micahhausler/aws-iam-policy/policy"
)

const (
	testOwnerAccount       = "916218008751"
	testResearcherAccount  = "123456789012"
	testSecondResearcher   = "210987654321"
	testRegion             = "ap-southeast-2"
	testDatasetURI         = "s3://ceph-source/datasets/cohort-a/"
	testBucket             = "hot-cache-cohort-a"
	testKeyArn             = "arn:aws:kms:ap-southeast-2:916218008751:key/1a2b3c4d-5e6f-7081-92a3-b4c5d6e7f809"
	testVpcID              = "vpc-0a1b2c3d4e5f60718"
	testSecondVpcID        = "vpc-0f1e2d3c4b5a69788"
	testCopyID             = "01J8ZQ4N8Z4T7VQ2K3M5X6Y7Z9"
	testGrantID            = "01J8ZQ4N8Z4T7VQ2K3M5X6Y7A0"
	testSecondGrantID      = "01J8ZQ4N8Z4T7VQ2K3M5X6Y7B1"
	testResearcherRoot     = "arn:aws:iam::" + testResearcherAccount + ":root"
	testSecondResearcherRt = "arn:aws:iam::" + testSecondResearcher + ":root"
)

func testSettings() PolicySettings {
	return PolicySettings{
		AccountID:          testOwnerAccount,
		Region:             testRegion,
		CopyWriterRoleName: "copy-writer",
		CustodianRoleARN:   "arn:aws:iam::916218008751:role/dataset-custodian",
	}
}

// grantFixture is a grant in the state that puts statements in the documents, with the
// expiry left to the caller.
func grantFixture(grantID, researcherAccountID, targetVpcID string, expiresAt *time.Time) *Grant {
	return &Grant{
		GrantID:             grantID,
		CopyID:              testCopyID,
		DatasetURI:          testDatasetURI,
		HotCacheBucket:      testBucket,
		KeyArn:              testKeyArn,
		ResearcherAccountID: researcherAccountID,
		TargetVpcID:         targetVpcID,
		State:               GrantStateGranted,
		CreatedAt:           time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC),
		ExpiresAt:           expiresAt,
	}
}

// grantedFixture is the first researcher's grant, which is the one most of these tests
// only need one of.
func grantedFixture(expiresAt *time.Time) *Grant {
	return grantFixture(testGrantID, testResearcherAccount, testVpcID, expiresAt)
}

// targetFixture is the bucket and key of one dataset's copy, carrying the given grants.
func targetFixture(grants ...*Grant) PolicyTarget {
	return PolicyTarget{
		DatasetURI:     testDatasetURI,
		HotCacheBucket: testBucket,
		KeyArn:         testKeyArn,
		Grants:         grants,
	}
}

func TestRenderPoliciesGrantsExactlyWhatTheGrantDescribes(t *testing.T) {
	expiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	grant := grantedFixture(&expiry)
	target := targetFixture(grant)

	policies, err := RenderPolicies(target, testSettings())
	if err != nil {
		t.Fatalf("RenderPolicies: %v", err)
	}
	ids := statementIDsFor(grant)

	read := findStatement(t, policies.Bucket, ids.read)
	if got := principalValues(t, read.Principal); len(got) != 1 || got[0] != testResearcherRoot {
		t.Errorf("grant principal: expected the researcher account root, got %v", got)
	}
	assertStringsEqual(t, "grant actions", []string{"s3:GetObject", "s3:ListBucket"}, read.Action.Values())
	assertStringsEqual(t, "grant resources",
		[]string{"arn:aws:s3:::hot-cache-cohort-a", "arn:aws:s3:::hot-cache-cohort-a/*"}, read.Resource.Values())
	if got := conditionValue(t, read, "StringEquals", "aws:SourceVpc"); got != testVpcID {
		t.Errorf("grant VPC condition: expected %q, got %q", testVpcID, got)
	}
	if got := conditionValue(t, read, "DateLessThan", "aws:CurrentTime"); got != "2026-10-01T06:30:00Z" {
		t.Errorf("grant expiry condition: got %q", got)
	}

	decrypt := findStatement(t, policies.Key, ids.decrypt)
	assertStringsEqual(t, "decrypt actions", []string{"kms:Decrypt", "kms:DescribeKey"}, decrypt.Action.Values())
	if got := conditionValue(t, decrypt, "StringEquals", "kms:ViaService"); got != "s3.ap-southeast-2.amazonaws.com" {
		t.Errorf("decrypt service condition: got %q", got)
	}
	if got := conditionValue(t, decrypt, "DateLessThan", "aws:CurrentTime"); got != "2026-10-01T06:30:00Z" {
		t.Errorf("decrypt expiry condition: got %q", got)
	}

	// The key is not asked about the VPC a request came from: S3 calls it on the
	// requester's behalf, from its own infrastructure.
	for _, st := range statements(policies.Key) {
		for _, operands := range st.Condition {
			for name := range operands {
				if strings.Contains(strings.ToLower(name), "vpc") {
					t.Errorf("key policy statement %q conditions on %q", st.Sid, name)
				}
			}
		}
	}

	// Reads are granted; nothing else is.
	for _, st := range statements(policies.Bucket) {
		if st.Effect != policy.EffectAllow {
			continue
		}
		for _, action := range st.Action.Values() {
			switch action {
			case "s3:GetObject", "s3:ListBucket":
			default:
				t.Errorf("bucket policy allows %q", action)
			}
		}
	}
}

// Two researchers reading one copy are two grants and two sets of statements, not one
// statement naming both accounts. Folding them together would give each researcher the
// other's VPC and the later of the two expiries, and would make withdrawing one a rewrite
// of the other.
func TestRenderPoliciesWriteOneSetOfStatementsPerGrant(t *testing.T) {
	firstExpiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	first := grantedFixture(&firstExpiry)
	second := grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil)
	target := targetFixture(first, second)

	policies, err := RenderPolicies(target, testSettings())
	if err != nil {
		t.Fatalf("RenderPolicies: %v", err)
	}

	for _, tc := range []struct {
		grant     *Grant
		root      string
		vpc       string
		expiry    string
		unlimited bool
	}{
		{grant: first, root: testResearcherRoot, vpc: testVpcID, expiry: "2026-10-01T06:30:00Z"},
		{grant: second, root: testSecondResearcherRt, vpc: testSecondVpcID, unlimited: true},
	} {
		t.Run(tc.grant.GrantID, func(t *testing.T) {
			ids := statementIDsFor(tc.grant)

			read := findStatement(t, policies.Bucket, ids.read)
			assertStringsEqual(t, "grant principals", []string{tc.root}, principalValues(t, read.Principal))
			if got := conditionValue(t, read, "StringEquals", "aws:SourceVpc"); got != tc.vpc {
				t.Errorf("grant VPC condition: expected %q, got %q", tc.vpc, got)
			}

			decrypt := findStatement(t, policies.Key, ids.decrypt)
			assertStringsEqual(t, "decrypt principals", []string{tc.root}, principalValues(t, decrypt.Principal))

			// The artefacts are withheld from each researcher by a deny of their own,
			// since one addressed to both would have to name a principal neither of them
			// is.
			deny := findStatement(t, policies.Bucket, ids.denyControl)
			assertStringsEqual(t, "control-prefix deny principals", []string{tc.root}, principalValues(t, deny.Principal))

			if tc.unlimited {
				for _, st := range []policy.Statement{read, decrypt} {
					if _, bound := st.Condition["DateLessThan"]; bound {
						t.Errorf("statement %q bounds an unlimited grant by time", st.Sid)
					}
				}
				return
			}
			for _, st := range []policy.Statement{read, decrypt} {
				if got := conditionValue(t, st, "DateLessThan", "aws:CurrentTime"); got != tc.expiry {
					t.Errorf("statement %q admits requests until %q, the grant records %q", st.Sid, got, tc.expiry)
				}
			}
		})
	}
}

// Withdrawing one grant is this same render with that grant left out, so the statements of
// every other grant have to come back byte-identical. A researcher losing access because
// somebody else's was withdrawn is the failure this rules out.
func TestRenderPoliciesLeaveTheRemainingGrantsUntouched(t *testing.T) {
	expiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	kept := grantedFixture(&expiry)
	withdrawn := grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil)

	both := targetFixture(kept, withdrawn)
	before, err := RenderPolicies(both, testSettings())
	if err != nil {
		t.Fatalf("rendering both grants: %v", err)
	}

	revoked := *withdrawn
	revoked.State = GrantStateRevoked
	after, err := RenderPolicies(targetFixture(kept, &revoked), testSettings())
	if err != nil {
		t.Fatalf("rendering with the second grant withdrawn: %v", err)
	}

	keptIDs := statementIDsFor(kept)
	for _, tc := range []struct {
		document string
		sid      string
		before   *policy.Policy
		after    *policy.Policy
	}{
		{"bucket policy", keptIDs.read, before.Bucket, after.Bucket},
		{"bucket policy", keptIDs.denyControl, before.Bucket, after.Bucket},
		{"key policy", keptIDs.decrypt, before.Key, after.Key},
	} {
		was := findStatement(t, tc.before, tc.sid)
		now := findStatement(t, tc.after, tc.sid)
		if !statementJSON(t, was).Equal(statementJSON(t, now)) {
			t.Errorf("withdrawing another grant changed %s statement %q", tc.document, tc.sid)
		}
	}

	// And the withdrawn grant is gone from both documents rather than merely disabled.
	withdrawnIDs := statementIDsFor(withdrawn)
	for _, sid := range []string{withdrawnIDs.read, withdrawnIDs.denyControl} {
		if _, found := statementBySid(after.Bucket, sid); found {
			t.Errorf("the bucket policy still carries statement %q of the withdrawn grant", sid)
		}
	}
	if _, found := statementBySid(after.Key, withdrawnIDs.decrypt); found {
		t.Errorf("the key policy still carries statement %q of the withdrawn grant", withdrawnIDs.decrypt)
	}
	for _, doc := range []*policy.Policy{after.Bucket, after.Key} {
		for _, st := range statements(doc) {
			for _, named := range principalValues(t, st.Principal) {
				if strings.Contains(named, testSecondResearcher) {
					t.Errorf("statement %q still names the withdrawn researcher: %q", st.Sid, named)
				}
			}
		}
	}
}

// The documents are rendered from a set of grants, and a set has no order. Ordering the
// statements by grant id is what makes the same grants render the same bytes, whichever
// order the ledger handed them back in -- and a document that reorders between renders is
// a difference reconciliation has to read as drift.
func TestRenderPoliciesOrderStatementsByGrant(t *testing.T) {
	expiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	first := grantedFixture(&expiry)
	second := grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil)

	inOrder := renderedPair(t, targetFixture(first, second))
	reversed := renderedPair(t, targetFixture(second, first))

	if inOrder.bucket != reversed.bucket {
		t.Errorf("the grants' order changed the bucket policy:\n%s\n%s", inOrder.bucket, reversed.bucket)
	}
	if inOrder.key != reversed.key {
		t.Errorf("the grants' order changed the key policy:\n%s\n%s", inOrder.key, reversed.key)
	}

	// Ordered by grant id, so where in the document a grant's statements land is a fact
	// about the grant rather than about how it was read.
	target := targetFixture(second, first)
	sids := make([]string, 0, 2)
	for _, st := range statements(mustRender(t, target).Bucket) {
		if st.Effect == policy.EffectAllow {
			sids = append(sids, st.Sid)
		}
	}
	assertStringsEqual(t, "granting statements",
		[]string{statementIDsFor(first).read, statementIDsFor(second).read}, sids)
}

func TestRenderPoliciesWithholdsCopyArtefactsFromTheResearcher(t *testing.T) {
	grant := grantedFixture(nil)
	target := targetFixture(grant)

	policies := mustRender(t, target)

	deny := findStatement(t, policies.Bucket, statementIDsFor(grant).denyControl)
	if deny.Effect != policy.EffectDeny {
		t.Errorf("control-prefix statement effect: got %q", deny.Effect)
	}
	// Scoped to the researcher: a deny addressed to everyone would take the artefacts
	// away from the copy that writes them, and from the owning account.
	if got := principalValues(t, deny.Principal); len(got) != 1 || got[0] != testResearcherRoot {
		t.Errorf("control-prefix deny principal: expected the researcher account root, got %v", got)
	}
	want := "arn:aws:s3:::hot-cache-cohort-a/" + ControlPrefix + "/*"
	assertStringsEqual(t, "control-prefix deny resources", []string{want}, deny.Resource.Values())
}

func TestRenderPoliciesOmitsTheTimeConditionForAnUnlimitedGrant(t *testing.T) {
	grant := grantedFixture(nil)
	target := targetFixture(grant)

	policies := mustRender(t, target)

	for _, doc := range []*policy.Policy{policies.Bucket, policies.Key} {
		for _, st := range statements(doc) {
			if _, found := st.Condition["DateLessThan"]; found {
				t.Errorf("statement %q carries a time condition for an unlimited grant", st.Sid)
			}
		}
	}

	// The grant itself is still rendered -- unlimited means no bound, not no access.
	ids := statementIDsFor(grant)
	findStatement(t, policies.Bucket, ids.read)
	findStatement(t, policies.Key, ids.decrypt)
}

// A grant whose expiry has passed keeps its statements: the time condition is what denies
// the reads, and leaving the statements in place is what makes extending enough to restore
// access.
func TestRenderPoliciesKeepsTheStatementsOfAnExpiredGrant(t *testing.T) {
	expiry := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	grant := grantedFixture(&expiry)
	target := targetFixture(grant)

	policies := mustRender(t, target)
	ids := statementIDsFor(grant)

	read := findStatement(t, policies.Bucket, ids.read)
	if got := conditionValue(t, read, "DateLessThan", "aws:CurrentTime"); got != "2020-01-01T00:00:00Z" {
		t.Errorf("expected the recorded expiry to be rendered as-is, got %q", got)
	}
	findStatement(t, policies.Key, ids.decrypt)
}

// Withdrawing is this same render with the grant left out, so a copy whose grants have all
// been revoked renders exactly what a copy nobody was ever granted renders.
func TestRenderPoliciesNameNoResearcherWhenNothingIsGranted(t *testing.T) {
	revoked := grantedFixture(nil)
	revoked.State = GrantStateRevoked
	revokedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	revoked.RevokedAt = &revokedAt

	withdrawn := renderedPair(t, targetFixture(revoked))
	neverGranted := renderedPair(t, targetFixture())

	if withdrawn.bucket != neverGranted.bucket || withdrawn.key != neverGranted.key {
		t.Errorf("a withdrawn grant renders something other than the baseline pair:\n%s\n%s",
			withdrawn.bucket, withdrawn.key)
	}

	policies := mustRender(t, targetFixture(revoked))
	for _, doc := range []*policy.Policy{policies.Bucket, policies.Key} {
		for _, st := range statements(doc) {
			for _, p := range principalValues(t, st.Principal) {
				if strings.Contains(p, testResearcherAccount) {
					t.Errorf("statement %q names the researcher: %q", st.Sid, p)
				}
			}
		}
	}

	// What remains is the baseline the deployment establishes, so the key stays
	// administrable and the copy can still write.
	findStatement(t, policies.Bucket, SidDenyInsecureTransport)
	findStatement(t, policies.Key, SidAccountKeyAdministration)
	findStatement(t, policies.Key, SidCopyWriterEncrypt)
}

// The owning account must keep its own access to its own bucket and key: the only
// statement addressed to every principal is the transport one, whose condition is about
// how a request travelled rather than who made it.
func TestRenderPoliciesLeavesTheOwningAccountsAccessIntact(t *testing.T) {
	policies := mustRender(t, targetFixture(grantedFixture(nil)))

	for _, doc := range []*policy.Policy{policies.Bucket, policies.Key} {
		for _, st := range statements(doc) {
			if st.Effect != policy.EffectDeny {
				continue
			}
			if !isGlobalPrincipal(t, st.Principal) {
				continue
			}
			if st.Sid != SidDenyInsecureTransport {
				t.Errorf("statement %q denies every principal", st.Sid)
				continue
			}
			for operator := range st.Condition {
				if operator != "Bool" {
					t.Errorf("the transport deny carries a %q condition, which would apply to the owning account too", operator)
				}
			}
		}
	}

	findStatement(t, policies.Key, SidAccountKeyAdministration)
}

func TestRenderPoliciesOmitsTheCustodianWhenNoneIsConfigured(t *testing.T) {
	settings := testSettings()
	settings.CustodianRoleARN = ""

	policies, err := RenderPolicies(targetFixture(grantedFixture(nil)), settings)
	if err != nil {
		t.Fatalf("RenderPolicies: %v", err)
	}

	for _, st := range statements(policies.Key) {
		if st.Sid == SidCustodianWithdraw {
			t.Fatal("a custodian statement was rendered with no custodian configured")
		}
	}
}

func TestRenderPoliciesRefusesInputThatWouldDenySilently(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*PolicyTarget, *Grant, *PolicySettings)
		contains string
	}{
		{
			name: "a VPC ARN where the bare identifier is compared",
			mutate: func(_ *PolicyTarget, g *Grant, _ *PolicySettings) {
				g.TargetVpcID = "arn:aws:ec2:ap-southeast-2:123456789012:vpc/vpc-0a1b2c3d4e5f60718"
			},
			contains: "bare vpc-",
		},
		{
			name:     "a researcher named by something other than an account id",
			mutate:   func(_ *PolicyTarget, g *Grant, _ *PolicySettings) { g.ResearcherAccountID = "researcher-account" },
			contains: "12-digit",
		},
		{
			name:     "a zero expiry standing in for no expiry",
			mutate:   func(_ *PolicyTarget, g *Grant, _ *PolicySettings) { g.ExpiresAt = &time.Time{} },
			contains: "zero expiry",
		},
		{
			name:     "a derived state, which would drop the statements of an expired grant",
			mutate:   func(_ *PolicyTarget, g *Grant, _ *PolicySettings) { g.State = GrantStateExpired },
			contains: "not a state a record is held in",
		},
		{
			name: "one grant deployed twice, whose statements could be matched to neither",
			mutate: func(target *PolicyTarget, g *Grant, _ *PolicySettings) {
				twin := *g
				target.Grants = append(target.Grants, &twin)
			},
			contains: "deployed twice",
		},
		{
			name:     "no copy writer role, which would strand the next copy",
			mutate:   func(_ *PolicyTarget, _ *Grant, s *PolicySettings) { s.CopyWriterRoleName = "" },
			contains: "copy writer role",
		},
		{
			name:     "no region for the service condition",
			mutate:   func(_ *PolicyTarget, _ *Grant, s *PolicySettings) { s.Region = "" },
			contains: "region",
		},
		{
			name: "a padded bucket name that would be rendered into an ARN as-is",
			mutate: func(target *PolicyTarget, _ *Grant, _ *PolicySettings) {
				target.HotCacheBucket = " " + testBucket
			},
			contains: "usable bucket name",
		},
		{
			name:     "no key, so there is nothing to write a key policy to",
			mutate:   func(target *PolicyTarget, _ *Grant, _ *PolicySettings) { target.KeyArn = "" },
			contains: "names no key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grant := grantedFixture(nil)
			target := targetFixture(grant)
			settings := testSettings()
			tt.mutate(&target, grant, &settings)

			_, err := RenderPolicies(target, settings)
			if err == nil {
				t.Fatal("expected the render to be refused")
			}
			if !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("error should mention %q, got: %v", tt.contains, err)
			}
		})
	}
}

// Everything wrong is reported at once. A target is the whole of a copy's access, so
// correcting one grant only to be told about the next is the same answer twice, and an
// operator repairing a ledger by hand has to see the shape of the problem in one go.
func TestRenderPoliciesReportEveryProblemAtOnce(t *testing.T) {
	first := grantedFixture(nil)
	first.ResearcherAccountID = "not-an-account"
	second := grantFixture(testSecondGrantID, testSecondResearcher, "vpc", nil)

	settings := testSettings()
	settings.Region = ""

	_, err := RenderPolicies(targetFixture(first, second), settings)
	if err == nil {
		t.Fatal("expected the render to be refused")
	}
	for _, want := range []string{"12-digit", "bare vpc-", "region"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got:\n%v", want, err)
		}
	}
}

// Whatever is rendered has to survive the round trip through JSON, since that is the
// form both AWS and the reconciliation read it in.
func TestRenderedPoliciesRoundTripThroughJSON(t *testing.T) {
	expiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	policies := mustRender(t, targetFixture(
		grantedFixture(&expiry),
		grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil),
	))

	for _, tc := range []struct {
		name string
		text func() (string, error)
		want *policy.Policy
	}{
		{name: "bucket", text: policies.BucketPolicyJSON, want: policies.Bucket},
		{name: "key", text: policies.KeyPolicyJSON, want: policies.Key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := tc.text()
			if err != nil {
				t.Fatalf("marshalling the %s policy: %v", tc.name, err)
			}

			var decoded policy.Policy
			if err := json.Unmarshal([]byte(text), &decoded); err != nil {
				t.Fatalf("the %s policy is not valid policy JSON: %v\n%s", tc.name, err, text)
			}
			if !decoded.Equal(tc.want) {
				t.Errorf("the %s policy did not survive a round trip:\n%s", tc.name, text)
			}
			if decoded.Version != policy.VersionLatest {
				t.Errorf("the %s policy declares version %q", tc.name, decoded.Version)
			}
		})
	}
}

func TestRenderPoliciesReadsThePartitionFromTheKey(t *testing.T) {
	grant := grantedFixture(nil)
	grant.KeyArn = "arn:aws-cn:kms:cn-north-1:916218008751:key/1a2b3c4d-5e6f-7081-92a3-b4c5d6e7f809"

	target := targetFixture(grant)
	target.KeyArn = grant.KeyArn

	settings := testSettings()
	settings.Region = "cn-north-1"
	settings.CustodianRoleARN = "arn:aws-cn:iam::916218008751:role/dataset-custodian"

	policies, err := RenderPolicies(target, settings)
	if err != nil {
		t.Fatalf("RenderPolicies: %v", err)
	}
	ids := statementIDsFor(grant)

	read := findStatement(t, policies.Bucket, ids.read)
	for _, resource := range read.Resource.Values() {
		if !strings.HasPrefix(resource, "arn:aws-cn:s3:::") {
			t.Errorf("resource %q was rendered in the wrong partition", resource)
		}
	}
	decrypt := findStatement(t, policies.Key, ids.decrypt)
	if got := conditionValue(t, decrypt, "StringEquals", "kms:ViaService"); got != "s3.cn-north-1.amazonaws.com.cn" {
		t.Errorf("service condition: got %q", got)
	}
}

// An expiry narrows when a grant admits a request and does nothing else. Rendering the
// same grant unlimited and expiring must therefore produce documents that differ in the
// time condition on that grant's granting statements and nowhere else -- so the expiry
// cannot quietly drop the service condition that confines the decrypt permission, attach
// itself to the deny that withholds the copy's artefacts, where it would lapse with the
// grant, or reach the statements of a grant it is nothing to do with.
func TestRenderPoliciesBindOnlyTheGrantToTheExpiry(t *testing.T) {
	expiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	other := grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil)

	unlimitedTarget := targetFixture(grantedFixture(nil), other)
	expiringTarget := targetFixture(grantedFixture(&expiry), other)

	unlimited := mustRender(t, unlimitedTarget)
	expiring := mustRender(t, expiringTarget)

	bound := statementIDsFor(grantedFixture(&expiry))
	for _, tc := range []struct {
		name      string
		unlimited *policy.Policy
		expiring  *policy.Policy
		bound     []string
	}{
		{name: "bucket", unlimited: unlimited.Bucket, expiring: expiring.Bucket, bound: []string{bound.read}},
		{name: "key", unlimited: unlimited.Key, expiring: expiring.Key, bound: []string{bound.decrypt}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var carrying []string
			for _, st := range statements(tc.expiring) {
				if _, found := st.Condition["DateLessThan"]; found {
					carrying = append(carrying, st.Sid)
				}
			}
			assertStringsEqual(t, "statements carrying the expiry", tc.bound, carrying)

			// The comparison below only means something if the time bound is what the
			// two documents differ by, so check that it is before stripping it.
			if tc.expiring.Equal(tc.unlimited) {
				t.Fatalf("the %s policy is unchanged by the expiry, so it bounds nothing", tc.name)
			}
			if !withoutTimeConditions(tc.expiring).Equal(tc.unlimited) {
				t.Errorf("the expiry changed the %s policy beyond its time condition", tc.name)
			}
		})
	}
}

// A condition compares a timestamp to the second in UTC, so that is the form an expiry
// is rendered in however it was recorded. Both documents must also render it identically:
// a bucket policy and a key policy disagreeing about when access ends would deny reads
// somewhere between the two.
func TestRenderPoliciesRenderTheExpiryAsWholeUTCSeconds(t *testing.T) {
	local := time.FixedZone("AEST", 10*60*60)
	expiry := time.Date(2026, 10, 1, 16, 30, 45, 987654321, local)

	grant := grantedFixture(&expiry)
	target := targetFixture(grant)
	policies := mustRender(t, target)
	ids := statementIDsFor(grant)

	const want = "2026-10-01T06:30:45Z"
	read := findStatement(t, policies.Bucket, ids.read)
	if got := conditionValue(t, read, "DateLessThan", "aws:CurrentTime"); got != want {
		t.Errorf("bucket policy expiry: expected %q, got %q", want, got)
	}
	decrypt := findStatement(t, policies.Key, ids.decrypt)
	if got := conditionValue(t, decrypt, "DateLessThan", "aws:CurrentTime"); got != want {
		t.Errorf("key policy expiry: expected %q, got %q", want, got)
	}
}

// A key policy names the key it is attached to as "*", so every statement is bounded by
// its conditions alone and by nothing else. This pins the whole condition surface rather
// than any one condition: an operand appearing here that no grant asked for narrows or
// widens access invisibly, and one comparing the VPC a request came from denies reads that
// did originate in the permitted VPC, since S3 calls the key from its own infrastructure
// on the requester's behalf.
func TestKeyPolicyBoundsEveryStatementByItsConditionsAlone(t *testing.T) {
	expiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	grant := grantedFixture(&expiry)
	unlimited := grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil)
	target := targetFixture(grant, unlimited)

	policies := mustRender(t, target)

	expected := map[string]struct {
		actions    []string
		conditions []string
	}{
		SidAccountKeyAdministration: {
			// Unconditional on purpose: a key whose administration lapses with a grant
			// cannot be corrected, or withdrawn.
			actions: []string{"kms:*"},
		},
		SidCopyWriterEncrypt: {
			actions: []string{
				"kms:GenerateDataKey", "kms:GenerateDataKeyWithoutPlaintext", "kms:Decrypt", "kms:DescribeKey",
			},
			conditions: []string{"StringLike/kms:EncryptionContext:aws:s3:arn"},
		},
		SidCustodianWithdraw: {
			// Withdrawal only. Undoing it needs the account's key administrators.
			actions: []string{"kms:DisableKey", "kms:ScheduleKeyDeletion", "kms:DescribeKey"},
		},
		statementIDsFor(grant).decrypt: {
			actions:    []string{"kms:Decrypt", "kms:DescribeKey"},
			conditions: []string{"DateLessThan/aws:CurrentTime", "StringEquals/kms:ViaService"},
		},
		statementIDsFor(unlimited).decrypt: {
			actions:    []string{"kms:Decrypt", "kms:DescribeKey"},
			conditions: []string{"StringEquals/kms:ViaService"},
		},
	}

	rendered := statements(policies.Key)
	if len(rendered) != len(expected) {
		t.Fatalf("key policy holds %d statements, expected %d", len(rendered), len(expected))
	}
	for _, st := range rendered {
		want, known := expected[st.Sid]
		if !known {
			t.Errorf("unexpected key policy statement %q", st.Sid)
			continue
		}
		assertStringsEqual(t, st.Sid+" actions", want.actions, st.Action.Values())
		assertStringsEqual(t, st.Sid+" conditions", want.conditions, conditionKeys(st))
		assertStringsEqual(t, st.Sid+" resources", []string{"*"}, st.Resource.Values())
	}

	// The copy's permission is tied to this bucket's objects, so the key is of no use to
	// it for writing anywhere else.
	writer := findStatement(t, policies.Key, SidCopyWriterEncrypt)
	want := "arn:aws:s3:::" + testBucket + "/*"
	if got := conditionValue(t, writer, "StringLike", "kms:EncryptionContext:aws:s3:arn"); got != want {
		t.Errorf("copy writer encryption context: expected %q, got %q", want, got)
	}
}

// The copy's own bookkeeping shares the bucket with the data, so the prefix is all that
// separates them and the deny is the whole of the separation.
func TestControlPrefixDenyIsUnconditionalAndReachesNothingElse(t *testing.T) {
	expiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)
	grant := grantedFixture(&expiry)
	second := grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil)
	target := targetFixture(grant, second)

	policies := mustRender(t, target)
	deny := findStatement(t, policies.Bucket, statementIDsFor(grant).denyControl)

	// Every action, since none of it is the researcher's to read, list or write. And no
	// condition: withholding the artefacts is not something that should lapse with the
	// expiry or depend on where a request came from.
	assertStringsEqual(t, "control-prefix deny actions", []string{"s3:*"}, deny.Action.Values())
	assertStringsEqual(t, "control-prefix deny conditions", nil, conditionKeys(deny))

	// Confined to the artefacts. A deny reaching the bucket or its objects would withhold
	// the dataset itself, which is the whole of what was granted.
	suffix := "/" + ControlPrefix + "/*"
	for _, resource := range deny.Resource.Values() {
		if !strings.HasSuffix(resource, suffix) {
			t.Errorf("the control-prefix deny reaches %q, beyond the copy's own artefacts", resource)
		}
	}

	// The prefix is named by one deny per granted researcher and by nothing else, so the
	// artefacts are withheld from them and from nobody else -- least of all the copy that
	// writes them.
	var mentioning []string
	for _, doc := range []*policy.Policy{policies.Bucket, policies.Key} {
		for _, st := range statements(doc) {
			for _, resource := range resourcesOf(st) {
				if strings.Contains(resource, ControlPrefix) {
					mentioning = append(mentioning, st.Sid)
					break
				}
			}
		}
	}
	slices.Sort(mentioning)
	want := []string{statementIDsFor(grant).denyControl, statementIDsFor(second).denyControl}
	slices.Sort(want)
	assertStringsEqual(t, "statements naming the control prefix", want, mentioning)
}

// The owning account has to keep working on its own bucket and key however many grants
// stand against a copy, because a rendered document replaces the deployed one whole. If a
// render can deny the account, the render that withdraws the last grant is the one that
// locks the account out of undoing it.
func TestRenderPoliciesNeverDenyTheOwningAccountOrTheCopy(t *testing.T) {
	ownerRoot := "arn:aws:iam::" + testOwnerAccount + ":root"
	copyWriter := "arn:aws:iam::" + testOwnerAccount + ":role/copy-writer"
	bucketARN := "arn:aws:s3:::" + testBucket

	for _, tc := range grantCombinations() {
		t.Run(tc.name, func(t *testing.T) {
			policies := mustRender(t, targetFixture(tc.grants...))

			admin := findStatement(t, policies.Key, SidAccountKeyAdministration)
			assertStringsEqual(t, "key administration principals", []string{ownerRoot}, principalValues(t, admin.Principal))
			assertStringsEqual(t, "key administration actions", []string{"kms:*"}, admin.Action.Values())
			assertStringsEqual(t, "key administration resources", []string{"*"}, admin.Resource.Values())
			assertStringsEqual(t, "key administration conditions", nil, conditionKeys(admin))

			// The copy keeps its data-key permission whatever is granted, so a withdrawn
			// or failed dataset can be staged again without reaching for the key policy by
			// hand.
			findStatement(t, policies.Key, SidCopyWriterEncrypt)

			for _, doc := range []*policy.Policy{policies.Bucket, policies.Key} {
				for _, st := range statements(doc) {
					if st.Effect != policy.EffectDeny {
						continue
					}
					for _, named := range principalValues(t, st.Principal) {
						if named == ownerRoot || named == copyWriter {
							t.Errorf("statement %q denies %q", st.Sid, named)
						}
					}
				}
			}

			// The one deny addressed to every principal covers the bucket and its objects
			// and turns solely on how the request travelled, which costs the owning
			// account nothing.
			transport := findStatement(t, policies.Bucket, SidDenyInsecureTransport)
			assertStringsEqual(t, "transport deny resources",
				[]string{bucketARN, bucketARN + "/*"}, transport.Resource.Values())
			assertStringsEqual(t, "transport deny conditions",
				[]string{"Bool/aws:SecureTransport"}, conditionKeys(transport))
		})
	}
}

// Reconciling a deployed document against a rendered one matches statements by identifier,
// since marshalling and the console are both free to reorder them. A repeated identifier
// makes that matching ambiguous, and an unnamed statement cannot be matched at all. Several
// grants in one document is where this stops being free: their statements have to be told
// apart by something, and the grant id is what does it.
func TestRenderedPoliciesIdentifyEveryStatementUniquely(t *testing.T) {
	for _, tc := range grantCombinations() {
		t.Run(tc.name, func(t *testing.T) {
			policies := mustRender(t, targetFixture(tc.grants...))

			for _, doc := range []*policy.Policy{policies.Bucket, policies.Key} {
				seen := map[string]bool{}
				for _, st := range statements(doc) {
					if st.Sid == "" {
						t.Errorf("a statement was rendered without an identifier")
						continue
					}
					if seen[st.Sid] {
						t.Errorf("statement identifier %q was rendered twice", st.Sid)
					}
					seen[st.Sid] = true
				}
			}
		})
	}
}

// Nothing rendered is nothing to apply. Both guards exist because applying an empty
// document would replace the deployed policy with one granting nobody anything, which is
// a withdrawal nobody asked for.
func TestPoliciesRefuseToStandInForARenderThatDidNotHappen(t *testing.T) {
	var unrendered Policies
	if _, err := unrendered.BucketPolicyJSON(); err == nil {
		t.Error("an unrendered bucket policy marshalled to a document that could be applied")
	}
	if _, err := unrendered.KeyPolicyJSON(); err == nil {
		t.Error("an unrendered key policy marshalled to a document that could be applied")
	}
}

// grantCombinations is what a copy's grants can amount to: none at all, one, two, and a
// mix of granted and withdrawn. The invariants that hold of every render are stated over
// all of them.
func grantCombinations() []struct {
	name   string
	grants []*Grant
} {
	revoked := grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil)
	revoked.State = GrantStateRevoked
	expiry := time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)

	return []struct {
		name   string
		grants []*Grant
	}{
		{name: "no grants at all", grants: nil},
		{name: "one grant", grants: []*Grant{grantedFixture(&expiry)}},
		{name: "two grants", grants: []*Grant{
			grantedFixture(&expiry),
			grantFixture(testSecondGrantID, testSecondResearcher, testSecondVpcID, nil),
		}},
		{name: "one granted and one withdrawn", grants: []*Grant{grantedFixture(nil), revoked}},
		{name: "only a withdrawn grant", grants: []*Grant{revoked}},
	}
}

// renderedDocumentPair is the two documents as they would be written.
type renderedDocumentPair struct {
	bucket string
	key    string
}

func mustRender(t *testing.T, target PolicyTarget) Policies {
	t.Helper()

	policies, err := RenderPolicies(target, testSettings())
	if err != nil {
		t.Fatalf("RenderPolicies: %v", err)
	}
	return policies
}

func renderedPair(t *testing.T, target PolicyTarget) renderedDocumentPair {
	t.Helper()

	policies := mustRender(t, target)
	bucket, err := policies.BucketPolicyJSON()
	if err != nil {
		t.Fatalf("rendering the bucket policy: %v", err)
	}
	key, err := policies.KeyPolicyJSON()
	if err != nil {
		t.Fatalf("rendering the key policy: %v", err)
	}
	return renderedDocumentPair{bucket: bucket, key: key}
}

func statements(doc *policy.Policy) []policy.Statement {
	if doc == nil || doc.Statements == nil {
		return nil
	}
	return doc.Statements.Values()
}

// statementBySid finds a statement without failing when it is absent, for the assertions
// about statements that must not have been rendered.
func statementBySid(doc *policy.Policy, sid string) (policy.Statement, bool) {
	for _, st := range statements(doc) {
		if st.Sid == sid {
			return st, true
		}
	}
	return policy.Statement{}, false
}

func findStatement(t *testing.T, doc *policy.Policy, sid string) policy.Statement {
	t.Helper()

	st, found := statementBySid(doc, sid)
	if !found {
		t.Fatalf("no statement %q in the rendered policy", sid)
	}
	return st
}

// statementJSON returns a statement as a one-statement document, so two of them can be
// compared for everything they say rather than field by field.
func statementJSON(t *testing.T, st policy.Statement) *policy.Policy {
	t.Helper()

	return &policy.Policy{
		Version:    policy.VersionLatest,
		Statements: policy.NewStatementOrSlice(st),
	}
}

func conditionValue(t *testing.T, st policy.Statement, operator, key string) string {
	t.Helper()

	operands, found := st.Condition[operator]
	if !found {
		t.Fatalf("statement %q has no %s condition", st.Sid, operator)
	}
	value, found := operands[key]
	if !found {
		t.Fatalf("statement %q has no %s condition on %s", st.Sid, operator, key)
	}
	strs, _, _ := value.Values()
	if len(strs) != 1 {
		t.Fatalf("statement %q condition %s/%s: expected one value, got %v", st.Sid, operator, key, strs)
	}
	return strs[0]
}

func principalValues(t *testing.T, p *policy.Principal) []string {
	t.Helper()

	if p == nil {
		return nil
	}
	if awsPrincipal := p.AWS(); awsPrincipal != nil {
		return awsPrincipal.Values()
	}
	// A bare-string principal (including "*") round-trips through its JSON form.
	raw, err := p.MarshalJSON()
	if err != nil {
		t.Fatalf("reading a principal: %v", err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return []string{s}
}

func isGlobalPrincipal(t *testing.T, p *policy.Principal) bool {
	t.Helper()

	values := principalValues(t, p)
	return len(values) == 1 && values[0] == policy.PrincipalAll
}

// conditionKeys lists what a statement is conditioned on, as "Operator/Key" pairs in a
// stable order. Comparing the whole list is what makes an unexpected condition a failure
// rather than something a test has to know to look for.
func conditionKeys(st policy.Statement) []string {
	var keys []string
	for operator, operands := range st.Condition {
		for key := range operands {
			keys = append(keys, operator+"/"+key)
		}
	}
	slices.Sort(keys)
	return keys
}

// withoutTimeConditions copies a document with every time bound removed, so two renders
// differing only in an expiry can be compared for everything else.
func withoutTimeConditions(doc *policy.Policy) *policy.Policy {
	stripped := make([]policy.Statement, 0, len(statements(doc)))
	for _, st := range statements(doc) {
		conditions := make(map[string]map[string]*policy.ConditionValue, len(st.Condition))
		for operator, operands := range st.Condition {
			if strings.HasPrefix(operator, "Date") {
				continue
			}
			conditions[operator] = operands
		}
		if len(conditions) == 0 {
			conditions = nil
		}
		st.Condition = conditions
		stripped = append(stripped, st)
	}

	return &policy.Policy{
		Id:         doc.Id,
		Version:    doc.Version,
		Statements: policy.NewStatementOrSlice(stripped...),
	}
}

func assertStringsEqual(t *testing.T, what string, want, got []string) {
	t.Helper()

	if len(want) != len(got) {
		t.Errorf("%s: expected %v, got %v", what, want, got)
		return
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("%s: expected %v, got %v", what, want, got)
			return
		}
	}
}
