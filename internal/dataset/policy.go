package dataset

// This file is the enforcement point of the S3 release mechanism. Nothing of ours sits
// in the read path, so what a researcher can and cannot do is entirely what these two
// documents say: the cache bucket's policy and the policy of the key encrypting it.
//
// Both documents are rendered whole from one target -- the bucket and key a dataset's
// copy lives in, together with the grants made against that copy -- and no other code
// composes either of them. That has two consequences worth stating, because the rest of
// the file only makes sense with them in mind:
//
//   - Every statement the bucket and the key need must appear here, including the
//     baseline statements the per-dataset deployment establishes. A rendered document
//     replaces whatever is deployed, so a statement omitted here is a statement
//     removed.
//   - Withdrawing is not a separate operation on the documents. It is this same render
//     with the withdrawn grant left out, which is why a copy whose grants have all been
//     revoked and a copy nobody was ever granted produce the same pair.
//
// Several grants may be live on one copy at once, and each contributes statements of its
// own rather than being folded into one statement naming several accounts. Two
// researchers reading the same copy from two VPCs until two different dates cannot be
// said in a single statement, and withdrawing one of them has to leave the other exactly
// as it was.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/micahhausler/aws-iam-policy/policy"
)

// ControlPrefix is the key prefix the copy orchestration keeps its own artefacts under
// -- its start marker and the summary of what it transferred. Those artefacts share the
// bucket with the data, so the prefix is what separates them, and the researcher is
// denied it: nothing below it is dataset content. It matches the prefix the per-dataset
// deployment reserves.
const ControlPrefix = "_control"

// Statement identifiers of the statements every render writes, whoever is granted. They
// are exported because applying and reconciling both need to speak about individual
// statements, and a statement matched by identifier survives the reordering that
// marshalling and the console are free to do.
const (
	// SidDenyInsecureTransport rejects requests that did not arrive over TLS.
	SidDenyInsecureTransport = "DenyInsecureTransport"

	// SidAccountKeyAdministration keeps the owning account able to administer the key.
	SidAccountKeyAdministration = "AllowAccountKeyAdministration" // pragma: allowlist secret
	// SidCopyWriterEncrypt lets the copy write encrypted objects into the bucket.
	SidCopyWriterEncrypt = "AllowCopyWriterToEncrypt"
	// SidCustodianWithdraw is the kill switch: disable the key, or schedule its
	// deletion.
	SidCustodianWithdraw = "AllowCustodianToWithdraw"
)

// The identifiers of the three statements one grant contributes are its prefix plus the
// grant's own id. Keying them on the grant is what lets several grants sit in one
// document: two statements sharing an identifier can be matched back to neither, so a
// fixed name per statement would collapse the moment a copy carried a second grant.
// Grant ids are ULIDs, so what results stays alphanumeric, which is all IAM permits of a
// Sid.
const (
	sidResearcherReadPrefix        = "AllowResearcher"
	sidDenyResearcherControlPrefix = "DenyResearcherControl"
	sidResearcherDecryptPrefix     = "AllowResearcherDecrypt"
)

// defaultPartition is used when no partition can be read from the key ARN.
const defaultPartition = "aws"

// PolicySettings holds the facts the two documents need that the ledger does not carry:
// who owns the bucket and the key, which region their requests are served from, and which
// principals other than the researchers must keep working.
//
// These come from configuration and from the per-dataset deployment rather than from any
// record, so they are the same for every dataset in one deployment.
type PolicySettings struct {
	// AccountID owns the cache bucket, the key, and the ledger. Its key administration
	// statement has to be rendered every time: without it the next write to the key
	// policy has no principal permitted to make it.
	AccountID string

	// Region is where the bucket is served from. It appears in the key policy as the
	// service condition on a researcher's decrypt permission, which is what stops that
	// permission being usable for anything but reading this bucket.
	Region string

	// CopyWriterRoleName is the role the copy orchestration writes through. S3 asks the
	// key for a data key on its behalf, so the key policy has to name it or staging
	// fails partway through a transfer that may already have run for hours.
	CopyWriterRoleName string

	// CopyWriterRoleAccountID owns that role. Defaults to AccountID, for a copy
	// orchestration installed alongside the cache bucket.
	CopyWriterRoleAccountID string

	// CustodianRoleARN is the principal permitted to withdraw the data by disabling the
	// key or scheduling its deletion. Optional: where no separate custodian exists the
	// statement is not written, and withdrawal stays reachable through the account's
	// own key administration.
	CustodianRoleARN string

	// Partition is the ARN partition. Optional -- it is read from the target's key ARN
	// when that ARN parses, since the key and the bucket are necessarily in the same
	// partition.
	Partition string
}

// PolicyTarget is the bucket and key one dataset's copy lives in, together with the
// grants whose statements belong in the documents.
//
// It is what a rendered pair is actually about. The bucket and the key are deployed per
// dataset and every copy of that dataset names the same pair, so the two documents are
// shared by every grant made against it; rendering from one grant at a time would have
// each render replacing the others' statements. The target is therefore the whole of
// what the documents have to say, gathered before anything is rendered.
type PolicyTarget struct {
	// DatasetURI identifies the dataset the copy is of. It names the target in what is
	// reported, and is not rendered into either document.
	DatasetURI string

	// HotCacheBucket holds the copy, and KeyArn is the key encrypting that bucket. The
	// bucket and object ARNs are built from the name verbatim, and the partition every
	// other ARN is rendered in is read from the key.
	HotCacheBucket string
	KeyArn         string

	// Grants are the grants made against the copy. Only those still deployed contribute
	// statements; the rest are carried so that the render is made from the whole of what
	// the ledger holds rather than from a subset a caller had to filter correctly.
	Grants []*Grant
}

// deployedGrants returns the grants whose statements belong in the documents, ordered by
// grant id.
//
// The order is what makes a target render byte-identical documents every time. Statements
// arriving in a different order each render would be a difference in the deployed
// document with nothing behind it, and reconciliation reads any difference as drift.
//
// An expired grant is included: expiry is a condition inside its statements rather than
// their removal, which is what makes moving the expiry forward enough to restore access.
func (t PolicyTarget) deployedGrants() []*Grant {
	deployed := make([]*Grant, 0, len(t.Grants))
	for _, grant := range t.Grants {
		if grant != nil && grant.IsDeployed() {
			deployed = append(deployed, grant)
		}
	}
	slices.SortFunc(deployed, func(a, b *Grant) int {
		return strings.Compare(a.GrantID, b.GrantID)
	})
	return deployed
}

// granting reports whether the documents this target renders admit anybody at all. A
// target with no deployed grant renders the baseline pair, which is what withdrawing the
// last grant leaves behind.
func (t PolicyTarget) granting() bool {
	return len(t.deployedGrants()) > 0
}

// deployedGrantIDs lists the grants a render writes statements for, in the order they are
// written.
func (t PolicyTarget) deployedGrantIDs() []string {
	deployed := t.deployedGrants()
	ids := make([]string, 0, len(deployed))
	for _, grant := range deployed {
		ids = append(ids, grant.GrantID)
	}
	return ids
}

// statementIDs are the identifiers of the three statements one grant contributes: its
// read grant and the deny that withholds the copy's artefacts from it in the bucket
// policy, and its decrypt permission in the key policy.
type statementIDs struct {
	read        string
	denyControl string
	decrypt     string
}

// statementIDsFor names one grant's statements. They depend on the grant alone, which is
// what lets the documents of a copy carry several grants without any of them having to
// know about the others.
func statementIDsFor(grant *Grant) statementIDs {
	return statementIDs{
		read:        sidResearcherReadPrefix + grant.GrantID,
		denyControl: sidDenyResearcherControlPrefix + grant.GrantID,
		decrypt:     sidResearcherDecryptPrefix + grant.GrantID,
	}
}

// Policies is the pair of documents a copy's access is enforced by. They are rendered
// together because they are only correct together: a grant in the bucket policy with no
// matching decrypt permission on the key reads as an access denial indistinguishable
// from an expired or wrongly scoped one.
type Policies struct {
	// Bucket is the cache bucket's policy.
	Bucket *policy.Policy
	// Key is the policy of the key encrypting that bucket.
	Key *policy.Policy
}

// BucketPolicyJSON renders the bucket policy as the JSON document S3 accepts.
func (p Policies) BucketPolicyJSON() (string, error) {
	return marshalPolicy("bucket", p.Bucket)
}

// KeyPolicyJSON renders the key policy as the JSON document KMS accepts.
func (p Policies) KeyPolicyJSON() (string, error) {
	return marshalPolicy("key", p.Key)
}

func marshalPolicy(kind string, doc *policy.Policy) (string, error) {
	if doc == nil {
		return "", fmt.Errorf("no %s policy was rendered", kind)
	}
	// Indented, because these documents are read by people in the console at least as
	// often as by anything else.
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("rendering the %s policy as JSON: %w", kind, err)
	}
	return string(data), nil
}

// RenderPolicies renders the bucket and key policies for one copy of one dataset.
//
// Only the grants still deployed against the copy are written. A revoked grant
// contributes nothing, which is the whole of what withdrawing it does to the documents;
// an expired one is written in full, carrying the expiry it recorded, because the time
// condition is what denies the requests and leaving the statements in place is what makes
// moving the expiry forward enough to restore access without copying the data again.
func RenderPolicies(target PolicyTarget, settings PolicySettings) (Policies, error) {
	settings = settings.resolve(target.KeyArn)
	if err := validatePolicyTarget(target, settings); err != nil {
		return Policies{}, err
	}

	deployed := target.deployedGrants()
	return Policies{
		Bucket: bucketPolicy(target, settings, deployed),
		Key:    keyPolicy(target, settings, deployed),
	}, nil
}

// resolve fills in what can be derived, so a caller supplies only what is genuinely
// deployment-specific.
func (s PolicySettings) resolve(keyARN string) PolicySettings {
	s.AccountID = strings.TrimSpace(s.AccountID)
	s.Region = strings.TrimSpace(s.Region)
	s.CopyWriterRoleName = strings.TrimSpace(s.CopyWriterRoleName)
	s.CopyWriterRoleAccountID = strings.TrimSpace(s.CopyWriterRoleAccountID)
	s.CustodianRoleARN = strings.TrimSpace(s.CustodianRoleARN)
	s.Partition = strings.TrimSpace(s.Partition)

	if s.CopyWriterRoleAccountID == "" {
		s.CopyWriterRoleAccountID = s.AccountID
	}
	if s.Partition == "" {
		s.Partition = partitionOf(keyARN)
	}
	return s
}

// partitionOf reads the partition from a key ARN, falling back to the commercial one.
// The bucket and the key are in the same partition as each other, so one readable ARN
// is enough to render the rest.
func partitionOf(keyARN string) string {
	if parsed, err := awsarn.Parse(strings.TrimSpace(keyARN)); err == nil && parsed.Partition != "" {
		return parsed.Partition
	}
	return defaultPartition
}

// validatePolicyTarget refuses to render documents that would be silently wrong.
//
// Every check here guards a failure that presents as an access denial hours later and
// indistinguishably from every other cause: a VPC identifier in the wrong form matches
// no request, an account identifier in the wrong form matches no principal, and a zero
// expiry denies everything. Rendering is the last point at which any of them is still
// legible.
//
// Everything wrong is reported together. The target is the whole of a copy's access, so
// a caller correcting one grant only to be told about the next has been given the same
// answer twice.
func validatePolicyTarget(target PolicyTarget, settings PolicySettings) error {
	problems := validateSettings(settings)

	if !isTrimmedNonEmpty(target.HotCacheBucket) {
		problems = append(problems, fmt.Errorf(
			"the copy names cache bucket %q, which is not a usable bucket name; the bucket and object ARNs are built from it verbatim",
			target.HotCacheBucket))
	}
	if strings.TrimSpace(target.KeyArn) == "" {
		problems = append(problems, errors.New("the copy names no key, so there is no key policy to render"))
	}
	problems = append(problems, validateGrants(target)...)

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("cannot render policies for dataset %q: %w", target.DatasetURI, errors.Join(problems...))
}

// validateSettings reports what is wrong with the deployment-wide facts both documents
// are rendered from.
func validateSettings(settings PolicySettings) []error {
	var problems []error

	if !isAccountID(settings.AccountID) {
		problems = append(problems, fmt.Errorf(
			"owning account %q is not a 12-digit AWS account id; the key policy has to name its own account or the key becomes unadministrable",
			settings.AccountID))
	}
	if settings.Region == "" {
		problems = append(problems, errors.New(
			"no region given; a researcher's decrypt permission is conditioned on S3 in one region and cannot be rendered without it"))
	}
	if settings.CopyWriterRoleName == "" {
		problems = append(problems, errors.New(
			"no copy writer role name given; without it the key policy denies the copy the data key it needs to write objects"))
	} else if !isAccountID(settings.CopyWriterRoleAccountID) {
		problems = append(problems, fmt.Errorf(
			"copy writer role account %q is not a 12-digit AWS account id", settings.CopyWriterRoleAccountID))
	}
	if settings.CustodianRoleARN != "" {
		if _, err := awsarn.Parse(settings.CustodianRoleARN); err != nil {
			problems = append(problems, fmt.Errorf(
				"custodian %q is not an ARN; the key policy names the principal permitted to withdraw the data and cannot guess at it: %w",
				settings.CustodianRoleARN, err))
		}
	}
	return problems
}

// validateGrants reports what is wrong with the grants a target carries.
//
// The account and the VPC are only checked on the grants that are actually written: a
// withdrawn grant keeps whatever it recorded and contributes no statement for it to be
// wrong in. The state and the expiry are checked on every grant, because both say
// whether a grant is written at all and a record that cannot answer that is a record
// nothing here can act on.
func validateGrants(target PolicyTarget) []error {
	var problems []error
	writing := make(map[string]bool, len(target.Grants))

	for i, grant := range target.Grants {
		if grant == nil {
			problems = append(problems, fmt.Errorf("grant %d of this copy is missing, so what it grants cannot be read", i))
			continue
		}
		if !grant.State.IsStored() {
			problems = append(problems, fmt.Errorf(
				"grant %q is in state %q, which is not a state a record is held in; render from the stored state so a passed expiry keeps its grant",
				grant.GrantID, grant.State))
		}
		if grant.ExpiresAt != nil && grant.ExpiresAt.IsZero() {
			problems = append(problems, fmt.Errorf(
				"grant %q carries a zero expiry; leave the expiry unset for an unlimited grant rather than recording a date that denies every request",
				grant.GrantID))
		}
		if !grant.IsDeployed() {
			continue
		}

		// Two deployed grants sharing an id render statements sharing an identifier,
		// which reconciliation can match back to neither.
		if writing[grant.GrantID] {
			problems = append(problems, fmt.Errorf(
				"grant %q is deployed twice against this copy; its statements would carry the same identifiers and could be matched to neither",
				grant.GrantID))
		}
		writing[grant.GrantID] = true

		if !isAccountID(grant.ResearcherAccountID) {
			problems = append(problems, fmt.Errorf(
				"grant %q is to researcher account %q, which is not a 12-digit AWS account id",
				grant.GrantID, grant.ResearcherAccountID))
		}
		if !isVpcID(grant.TargetVpcID) {
			problems = append(problems, fmt.Errorf(
				"grant %q names target VPC %q, which is not a bare vpc- identifier; the bucket policy compares this value literally against the VPC a request arrived from, so an ARN here denies every read",
				grant.GrantID, grant.TargetVpcID))
		}
	}
	return problems
}

// isTrimmedNonEmpty reports whether s carries a value that can be rendered as-is.
// Padding is rejected rather than trimmed: a value that goes into an ARN has to be the
// value the record holds, or the ledger describes access that was never applied.
func isTrimmedNonEmpty(s string) bool {
	return s != "" && strings.TrimSpace(s) == s
}

// isAccountID reports whether s is an AWS account id: exactly twelve digits. Anything
// else in a principal -- an ARN, a name, a padded string -- names no account.
func isAccountID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isVpcID reports whether s is a bare VPC identifier. The two mechanisms in this project
// take a VPC in two different forms -- an ARN for the request-time lookup, a bare
// identifier for the policy condition -- so the wrong one arriving here is a plausible
// mistake with an invisible consequence.
func isVpcID(s string) bool {
	return strings.HasPrefix(s, "vpc-") && len(s) > len("vpc-")
}

// bucketPolicy renders the cache bucket's policy: two statements per deployed grant, and
// the transport deny the deployment establishes.
func bucketPolicy(target PolicyTarget, settings PolicySettings, deployed []*Grant) *policy.Policy {
	bucketARN := s3BucketARN(settings.Partition, target.HotCacheBucket)
	objectsARN := bucketARN + "/*"

	statements := make([]policy.Statement, 0, 2*len(deployed)+1)
	for _, grant := range deployed {
		ids := statementIDsFor(grant)
		statements = append(statements,
			researcherReadStatement(ids.read, grant, settings, bucketARN, objectsARN),
			denyControlArtefactsStatement(ids.denyControl, grant, settings, bucketARN),
		)
	}
	statements = append(statements, denyInsecureTransportStatement(bucketARN, objectsARN))

	return &policy.Policy{
		Version:    policy.VersionLatest,
		Statements: policy.NewStatementOrSlice(statements...),
	}
}

// researcherReadStatement is one grant. Everything a grant gives away is here and nowhere
// else.
//
// The principal is the researcher's account root, which delegates to their own IAM the
// question of which of their roles may read; the VPC condition is what actually bounds
// the grant, so widening the principal does not widen where reads can come from. The
// actions are reads only, and the bucket is unversioned, so there is no second
// object-version action to withhold.
func researcherReadStatement(sid string, grant *Grant, settings PolicySettings, bucketARN, objectsARN string) policy.Statement {
	conditions := map[string]map[string]*policy.ConditionValue{
		// The bare VPC identifier rather than the endpoint identifier: it is what the
		// grant is actually about, and it survives the researcher recreating their
		// endpoint.
		"StringEquals": {
			"aws:SourceVpc": policy.NewConditionValueString(true, grant.TargetVpcID),
		},
	}
	addExpiryCondition(conditions, grant)

	// ListBucket applies to the bucket ARN and GetObject to the object ARNs; each
	// action simply never matches the resource that is not its own.
	return policy.Statement{
		Sid:       sid,
		Effect:    policy.EffectAllow,
		Principal: policy.NewAWSPrincipal(accountRootARN(settings.Partition, grant.ResearcherAccountID)),
		Action:    policy.NewStringOrSlice(false, "s3:GetObject", "s3:ListBucket"),
		Resource:  policy.NewStringOrSlice(false, bucketARN, objectsARN),
		Condition: conditions,
	}
}

// denyControlArtefactsStatement withholds the copy orchestration's bookkeeping from one
// granted researcher, so a grant over the bucket cannot be read as a grant over how the
// copy ran or what it cost.
//
// Scoped to that researcher's own principal, and deliberately not to every principal: a
// Deny addressed to everyone would take the artefacts away from the copy that writes
// them, and from the researchers holding the copy's other grants. It carries no
// condition, so it holds regardless of where the request came from or whether the grant
// has expired.
func denyControlArtefactsStatement(sid string, grant *Grant, settings PolicySettings, bucketARN string) policy.Statement {
	return policy.Statement{
		Sid:       sid,
		Effect:    policy.EffectDeny,
		Principal: policy.NewAWSPrincipal(accountRootARN(settings.Partition, grant.ResearcherAccountID)),
		Action:    policy.NewStringOrSlice(true, "s3:*"),
		Resource:  policy.NewStringOrSlice(true, fmt.Sprintf("%s/%s/*", bucketARN, ControlPrefix)),
	}
}

// denyInsecureTransportStatement rejects requests that did not arrive over TLS. The
// per-dataset deployment establishes it and this is where it is preserved, since a
// rendered document replaces the deployed one whole.
//
// It is the only statement addressed to every principal, and the only one that may be:
// its condition is about how a request travelled, not about who made it or when. A Deny
// addressed to everyone and conditioned on a target VPC or on an expiry would read as a
// tighter version of the same idea and would in fact lock the owning account out of its
// own bucket, so those conditions belong only on statements naming one researcher.
func denyInsecureTransportStatement(bucketARN, objectsARN string) policy.Statement {
	return policy.Statement{
		Sid:       SidDenyInsecureTransport,
		Effect:    policy.EffectDeny,
		Principal: policy.NewGlobalPrincipal(),
		Action:    policy.NewStringOrSlice(true, "s3:*"),
		Resource:  policy.NewStringOrSlice(false, bucketARN, objectsARN),
		Condition: map[string]map[string]*policy.ConditionValue{
			"Bool": {
				"aws:SecureTransport": policy.NewConditionValueBool(true, false),
			},
		},
	}
}

// keyPolicy renders the policy of the key encrypting the cache bucket: the statements
// that keep the key administrable and writable, and one decrypt permission per deployed
// grant.
//
// Reads of the bucket are decrypted per request -- bucket keys are off on purpose, so
// nothing is cached that would let a read skip this document -- which is what makes
// disabling the key an immediate kill switch and makes the expiry conditions here
// meaningful.
func keyPolicy(target PolicyTarget, settings PolicySettings, deployed []*Grant) *policy.Policy {
	statements := []policy.Statement{
		accountKeyAdministrationStatement(settings),
		copyWriterStatement(target.HotCacheBucket, settings),
	}
	if settings.CustodianRoleARN != "" {
		statements = append(statements, custodianStatement(settings))
	}
	for _, grant := range deployed {
		statements = append(statements, researcherDecryptStatement(statementIDsFor(grant).decrypt, grant, settings))
	}

	return &policy.Policy{
		Version:    policy.VersionLatest,
		Statements: policy.NewStatementOrSlice(statements...),
	}
}

// accountKeyAdministrationStatement keeps the owning account in control of the key.
//
// It is rendered every time and unconditionally. A key policy that names no principal
// able to write the next key policy cannot be corrected, so this statement is what keeps
// every later render possible -- including the one that withdraws.
func accountKeyAdministrationStatement(settings PolicySettings) policy.Statement {
	return policy.Statement{
		Sid:       SidAccountKeyAdministration,
		Effect:    policy.EffectAllow,
		Principal: policy.NewAWSPrincipal(accountRootARN(settings.Partition, settings.AccountID)),
		Action:    policy.NewStringOrSlice(true, "kms:*"),
		Resource:  policy.NewStringOrSlice(true, "*"),
	}
}

// copyWriterStatement lets the copy orchestration's role have S3 encrypt the objects it
// writes.
//
// The encryption context condition ties that permission to this bucket's objects, so the
// role cannot use the key to write anywhere else. It matches the statement the
// per-dataset deployment establishes, so re-rendering the document leaves the copy's
// access exactly as deployed.
func copyWriterStatement(hotCacheBucket string, settings PolicySettings) policy.Statement {
	roleARN := fmt.Sprintf("arn:%s:iam::%s:role/%s",
		settings.Partition, settings.CopyWriterRoleAccountID, settings.CopyWriterRoleName)

	return policy.Statement{
		Sid:       SidCopyWriterEncrypt,
		Effect:    policy.EffectAllow,
		Principal: policy.NewAWSPrincipal(roleARN),
		Action: policy.NewStringOrSlice(false,
			"kms:GenerateDataKey", "kms:GenerateDataKeyWithoutPlaintext", "kms:Decrypt", "kms:DescribeKey"),
		Resource: policy.NewStringOrSlice(true, "*"),
		Condition: map[string]map[string]*policy.ConditionValue{
			"StringLike": {
				"kms:EncryptionContext:aws:s3:arn": policy.NewConditionValueString(true,
					s3BucketARN(settings.Partition, hotCacheBucket)+"/*"),
			},
		},
	}
}

// custodianStatement is the kill switch: stop the key working, or destroy it and with it
// the cached copy.
//
// Cancelling a scheduled deletion is deliberately absent. Withdrawing the data is the
// custodian's to do alone; undoing that decision needs the account's key administrators.
func custodianStatement(settings PolicySettings) policy.Statement {
	return policy.Statement{
		Sid:       SidCustodianWithdraw,
		Effect:    policy.EffectAllow,
		Principal: policy.NewAWSPrincipal(settings.CustodianRoleARN),
		Action:    policy.NewStringOrSlice(false, "kms:DisableKey", "kms:ScheduleKeyDeletion", "kms:DescribeKey"),
		Resource:  policy.NewStringOrSlice(true, "*"),
	}
}

// researcherDecryptStatement lets one grant's reads be decrypted.
//
// The service condition is what confines the permission to reading this bucket through
// S3: the key is useless to the researcher for anything else. There is deliberately no
// VPC condition here. When S3 decrypts on a requester's behalf the call to the key comes
// from S3's own infrastructure rather than from the requester's VPC, so a VPC condition
// on the key can deny a request that did originate in the permitted VPC. The VPC bound
// is the bucket policy's to enforce, and it is enforced before any decryption is asked
// for.
func researcherDecryptStatement(sid string, grant *Grant, settings PolicySettings) policy.Statement {
	conditions := map[string]map[string]*policy.ConditionValue{
		"StringEquals": {
			"kms:ViaService": policy.NewConditionValueString(true, s3ServiceHost(settings.Partition, settings.Region)),
		},
	}
	addExpiryCondition(conditions, grant)

	return policy.Statement{
		Sid:       sid,
		Effect:    policy.EffectAllow,
		Principal: policy.NewAWSPrincipal(accountRootARN(settings.Partition, grant.ResearcherAccountID)),
		Action:    policy.NewStringOrSlice(false, "kms:Decrypt", "kms:DescribeKey"),
		Resource:  policy.NewStringOrSlice(true, "*"),
		Condition: conditions,
	}
}

// addExpiryCondition adds the time bound to a granting statement, or adds nothing at
// all when the grant is unlimited.
//
// An unlimited grant carries no time condition rather than a distant date. A date would
// be a lie about what was agreed, would eventually arrive, and would make an unlimited
// grant indistinguishable from a long one when the deployed document is read back.
func addExpiryCondition(conditions map[string]map[string]*policy.ConditionValue, grant *Grant) {
	if grant.IsUnlimited() {
		return
	}
	conditions["DateLessThan"] = map[string]*policy.ConditionValue{
		"aws:CurrentTime": policy.NewConditionValueString(true, policyTime(*grant.ExpiresAt)),
	}
}

// policyTime formats an expiry the way a policy condition compares it: UTC, to the
// second. Sub-second precision is dropped, which can only ever extend access by less
// than a second and keeps a re-rendered document byte-identical to the deployed one.
func policyTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// accountRootARN names an account as a principal. Granting the account root delegates
// to that account's own IAM which of its principals may act.
func accountRootARN(partition, accountID string) string {
	return fmt.Sprintf("arn:%s:iam::%s:root", partition, accountID)
}

// s3BucketARN names a bucket. S3 bucket ARNs carry no region or account.
func s3BucketARN(partition, bucket string) string {
	return fmt.Sprintf("arn:%s:s3:::%s", partition, bucket)
}

// s3ServiceHost is the hostname S3 identifies itself by when it calls the key on a
// requester's behalf, which is the value a researcher's decrypt permission is
// conditioned on.
func s3ServiceHost(partition, region string) string {
	if partition == "aws-cn" {
		return fmt.Sprintf("s3.%s.amazonaws.com.cn", region)
	}
	return fmt.Sprintf("s3.%s.amazonaws.com", region)
}
