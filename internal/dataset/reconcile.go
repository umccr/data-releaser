package dataset

// This file answers one question: is the access the ledger describes the access that is
// actually deployed?
//
// It is needed because enforcement lives in two documents a person can edit in a
// console. The records declare intended access; the deployed documents are what a
// request is checked against. The two can therefore diverge, and divergence in either
// direction is invisible from outside -- a grant added by hand is data being read by
// someone the ledger has never heard of, and a grant removed by hand is an access denial
// with no cause the researcher or the operator can see.
//
// The comparison is deliberately not textual. AWS does not hand a document back the way
// it was written: a one-element list returns as a bare string, statements return in any
// order, a principal written as an account root ARN may return as a bare account id, and
// a console edit may respell any of it again. So what is compared is what each document
// grants -- which accounts may read, which VPCs they may read from, and until when.
// Those three are the whole of what a grant gives away, and everything else a document
// says is left alone.
//
// The three are compared as three independent sets rather than as a pairing of account
// with VPC with expiry. With a single grant against a copy that loses nothing: any
// deployed document pairing them differently has to name a second account or a second VPC
// to do it, and the second one is reported as soon as it appears. With several grants it
// does lose the pairing -- a document handing one grant's VPC to another grant's account
// states only values the ledger already describes, and so compares clean.
//
// That is the trade for a comparison that survives how AWS and a console hand a document
// back. Pairing the three would mean deciding which deployed statement stands for which
// grant, and a statement identifier is the only thing that could say so -- which is
// exactly what a console edit is free to change. What the sets still catch is a value
// nobody described: an account, a VPC or an expiry no grant names is reported the moment
// it appears, and that is the drift that actually happens, whether it was edited in by
// hand or left behind by a grant since withdrawn.
//
// What this cannot do is decide whether a request would succeed. A Deny elsewhere in the
// document, or in a researcher's own permissions, is not accounted for. A match means
// the deployed documents describe the access the ledger does, not that a researcher can
// certainly read.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/micahhausler/aws-iam-policy/policy"

	internalaws "github.com/umccr/data-releaser/internal/aws"
	"github.com/umccr/data-releaser/internal/iampolicy"
)

// DiffAspect names the part of a copy's access a difference is about. The three are
// what a report has to keep apart: who may read, where they may read from, and until
// when.
type DiffAspect string

const (
	// AspectGrantedAccount is an account a document admits.
	AspectGrantedAccount DiffAspect = "granted account"
	// AspectPermittedVpc is a VPC a document admits requests from.
	AspectPermittedVpc DiffAspect = "permitted VPC"
	// AspectExpiry is the time a document admits requests until.
	AspectExpiry DiffAspect = "expiry"
)

func (a DiffAspect) String() string {
	return string(a)
}

// aspectOrder is the order a report reads in: who, then where from, then until when.
var aspectOrder = []DiffAspect{AspectGrantedAccount, AspectPermittedVpc, AspectExpiry}

// The facts a granting statement states by saying nothing at all. Each is a value in its
// own right rather than an absence, so that a statement which lost a condition is
// reported as differing from one that carries it, rather than as having nothing to
// compare.
const (
	// noExpiry is the expiry of a grant carrying no time bound: access does not lapse.
	noExpiry = "no expiry"
	// anyVpc is the permitted VPC of a grant carrying no VPC condition: requests are
	// admitted wherever they arrive from.
	anyVpc = "any VPC"
	// everyPrincipal is the granted account of a statement addressed to everyone.
	everyPrincipal = "every principal"
)

// The condition keys the compared access is expressed with.
const (
	sourceVpcConditionKey   = "aws:SourceVpc"
	requestTimeConditionKey = "aws:CurrentTime"
)

// timeCeilingOperators are the condition operators that put a ceiling on the request
// time. A lower bound is not an expiry and is not read as one.
var timeCeilingOperators = []string{"DateLessThan", "DateLessThanEquals"}

// noSuchBucketPolicyCode is what S3 answers with when a bucket carries no policy at all.
const noSuchBucketPolicyCode = "NoSuchBucketPolicy"

// DeployedPolicies is the pair of documents as AWS currently holds them. A nil document
// stands for one that is not deployed at all, which grants nothing.
type DeployedPolicies struct {
	// Bucket is the deployed policy of the cache bucket.
	Bucket *policy.Policy
	// Key is the deployed policy of the key encrypting that bucket.
	Key *policy.Policy
}

// PolicyDifference is one way in which the deployed access differs from the access the
// ledger describes.
type PolicyDifference struct {
	// Document is which of the two documents the difference was found in.
	Document ApplyTarget

	// Aspect is the part of the access that differs.
	Aspect DiffAspect

	// Expected is what the record describes. Empty when the deployed document carries
	// something the record describes nothing for.
	Expected string

	// Deployed is what is in force. Empty when the record describes something the
	// deployed document does not carry.
	Deployed string

	// Statement identifies the deployed statement the difference was read from, where
	// that statement carries an identifier. Statements are matched by what they grant
	// rather than by identifier, so this only points an operator at the right part of
	// the document.
	Statement string
}

func (d PolicyDifference) String() string {
	var b strings.Builder
	switch {
	case d.Expected != "" && d.Deployed != "":
		fmt.Fprintf(&b, "%s: %s differs; the ledger describes %s, the deployed policy has %s",
			d.Document, d.Aspect, d.Expected, d.Deployed)
	case d.Expected != "":
		fmt.Fprintf(&b, "%s: the ledger describes %s %s, which the deployed policy does not grant",
			d.Document, d.Aspect, d.Expected)
	default:
		fmt.Fprintf(&b, "%s: the deployed policy has %s %s, which the ledger does not describe",
			d.Document, d.Aspect, d.Deployed)
	}
	if d.Statement != "" {
		fmt.Fprintf(&b, " (statement %q)", d.Statement)
	}
	return b.String()
}

// PolicyDiff is what comparing one copy's grants against its deployed documents found.
type PolicyDiff struct {
	// DatasetURI, HotCacheBucket, and KeyArn identify what was compared.
	DatasetURI     string
	HotCacheBucket string
	KeyArn         string

	// GrantIDs are the grants whose statements the compared documents were expected to
	// carry. Empty when the ledger describes no live grant against the copy, which is
	// the comparison worth making: what is deployed then has to grant nothing too.
	GrantIDs []string

	// Differences is every way the deployed access differs from the described access, in
	// document and then aspect order.
	Differences []PolicyDifference

	// Absent lists the documents that carry no policy at all. Every grant the ledger
	// describes for such a document is reported as a difference, and the absence is
	// worth naming besides: a bucket with no policy has also lost the baseline
	// statements its deployment establishes, and those are not part of the access
	// compared here.
	Absent []ApplyTarget
}

// Matches reports whether the deployed documents describe the access the ledger does.
func (d *PolicyDiff) Matches() bool {
	return d != nil && len(d.Differences) == 0 && len(d.Absent) == 0
}

// Report renders what was found as lines an operator reads.
func (d *PolicyDiff) Report() string {
	if d == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dataset %q", d.DatasetURI)
	if len(d.GrantIDs) > 0 {
		fmt.Fprintf(&b, " (grant %s)", strings.Join(d.GrantIDs, ", "))
	}
	if d.Matches() {
		b.WriteString(": the deployed policies describe the access the ledger does")
		return b.String()
	}
	for _, target := range d.Absent {
		switch target {
		case ApplyTargetBucket:
			fmt.Fprintf(&b, "\n  - the %s of %s is not deployed at all, so it grants nothing "+
				"and no longer refuses requests that arrive without TLS", target, d.HotCacheBucket)
		default:
			fmt.Fprintf(&b, "\n  - the %s of %s is not deployed at all, so it grants nothing",
				target, d.KeyArn)
		}
	}
	for _, difference := range d.Differences {
		fmt.Fprintf(&b, "\n  - %s", difference)
	}
	return b.String()
}

// PolicyDiffer reads the deployed documents and compares them with what the ledger
// describes.
type PolicyDiffer struct {
	objects  internalaws.S3Client
	keys     internalaws.KMSClient
	settings PolicySettings
}

// NewPolicyDiffer returns a differ that renders the expected documents with the given
// settings, which have to be the settings the deployed ones were applied with.
func NewPolicyDiffer(settings PolicySettings, objects internalaws.S3Client, keys internalaws.KMSClient) *PolicyDiffer {
	return &PolicyDiffer{
		objects:  objects,
		keys:     keys,
		settings: settings,
	}
}

// Diff reads both deployed documents and reports how the access they
// describe differs from the access the target's grants describe.
//
// A target granting nothing -- every grant against the copy withdrawn, or none ever made
// -- is compared just the same, and that is the case worth having: a grant still deployed
// after its withdrawal is access nobody believes exists.
func (d *PolicyDiffer) Diff(ctx context.Context, target PolicyTarget) (*PolicyDiff, error) {
	deployed, err := d.DeployedPolicies(ctx, target)
	if err != nil {
		return nil, err
	}
	return CompareDeployedPolicies(target, d.settings, deployed)
}

// DeployedPolicies reads the pair of documents currently in force for the target's
// dataset.
func (d *PolicyDiffer) DeployedPolicies(ctx context.Context, target PolicyTarget) (DeployedPolicies, error) {
	bucketPolicy, err := d.deployedBucketPolicy(ctx, target)
	if err != nil {
		return DeployedPolicies{}, err
	}
	keyPolicy, err := d.deployedKeyPolicy(ctx, target)
	if err != nil {
		return DeployedPolicies{}, err
	}
	return DeployedPolicies{Bucket: bucketPolicy, Key: keyPolicy}, nil
}

// deployedBucketPolicy reads the cache bucket's policy, reporting a bucket that carries
// no policy as an absent document rather than as a failure.
func (d *PolicyDiffer) deployedBucketPolicy(ctx context.Context, target PolicyTarget) (*policy.Policy, error) {
	output, err := d.objects.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{
		Bucket: aws.String(target.HotCacheBucket),
	})
	if err != nil {
		if policyIsAbsent(err) {
			return nil, nil
		}
		return nil, CacheBucketPrerequisite(target.HotCacheBucket, d.settings.AccountID).
			Missing(fmt.Errorf("reading the deployed policy of the bucket holding dataset %q: %w",
				target.DatasetURI, err))
	}
	if output.Policy == nil || strings.TrimSpace(*output.Policy) == "" {
		return nil, nil
	}
	return parseDeployedPolicy(ApplyTargetBucket, target.HotCacheBucket, *output.Policy)
}

// deployedKeyPolicy reads the policy of the key encrypting the cache bucket.
//
// KMS keeps a policy for every key it holds, so an empty answer is not something a real
// key produces. It is treated as an absent document rather than as a malformed one,
// since that still leaves the ledger's own grants comparable against nothing.
func (d *PolicyDiffer) deployedKeyPolicy(ctx context.Context, target PolicyTarget) (*policy.Policy, error) {
	output, err := d.keys.GetKeyPolicy(ctx, &kms.GetKeyPolicyInput{
		KeyId:      aws.String(target.KeyArn),
		PolicyName: aws.String(keyPolicyName),
	})
	if err != nil {
		return nil, DatasetKeyPrerequisite(target.KeyArn, d.settings.AccountID).
			Missing(fmt.Errorf("reading the deployed policy of the key encrypting dataset %q: %w",
				target.DatasetURI, err))
	}
	if output.Policy == nil || strings.TrimSpace(*output.Policy) == "" {
		return nil, nil
	}
	return parseDeployedPolicy(ApplyTargetKey, target.KeyArn, *output.Policy)
}

// policyIsAbsent reports whether the error says the document does not exist, as opposed
// to saying it could not be read. The distinction matters: a bucket with no policy
// grants nothing, which is a state worth comparing, whereas a bucket whose policy cannot
// be read leaves the deployed access unknown.
//
// The code is matched through the interface the SDK's errors satisfy, since S3 does not
// model this one as a type of its own.
func policyIsAbsent(err error) bool {
	var coded interface{ ErrorCode() string }
	return errors.As(err, &coded) && coded.ErrorCode() == noSuchBucketPolicyCode
}

// parseDeployedPolicy reads a deployed document.
//
// A document that cannot be read is an error rather than a difference: nothing can be
// said about the access it describes, and reporting it as granting nothing would read as
// a missing grant when it may be the opposite.
func parseDeployedPolicy(target ApplyTarget, subject, document string) (*policy.Policy, error) {
	var parsed policy.Policy
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		return nil, fmt.Errorf(
			"the deployed %s of %s cannot be read as a policy document, so the access it "+
				"describes cannot be compared with the ledger: %w", target, subject, err)
	}
	return &parsed, nil
}

// CompareDeployedPolicies compares documents already in hand against the pair the
// target's grants describe, for callers that have read them themselves.
//
// The expected pair is rendered rather than described here, so the comparison tracks
// whatever the renderer does and cannot drift from it.
func CompareDeployedPolicies(target PolicyTarget, settings PolicySettings, deployed DeployedPolicies) (*PolicyDiff, error) {
	expected, err := RenderPolicies(target, settings)
	if err != nil {
		return nil, err
	}

	diff := &PolicyDiff{
		DatasetURI:     target.DatasetURI,
		HotCacheBucket: target.HotCacheBucket,
		KeyArn:         target.KeyArn,
		GrantIDs:       target.deployedGrantIDs(),
	}
	if deployed.Bucket == nil {
		diff.Absent = append(diff.Absent, ApplyTargetBucket)
	}
	if deployed.Key == nil {
		diff.Absent = append(diff.Absent, ApplyTargetKey)
	}

	diff.Differences = append(diff.Differences, compareAccess(ApplyTargetBucket,
		observeBucketAccess(expected.Bucket, target.HotCacheBucket),
		observeBucketAccess(deployed.Bucket, target.HotCacheBucket))...)
	diff.Differences = append(diff.Differences, compareAccess(ApplyTargetKey,
		observeKeyAccess(expected.Key),
		observeKeyAccess(deployed.Key))...)

	return diff, nil
}

// grantObservation is one fact a document states about the access it grants: an account
// it admits, a VPC it admits requests from, or a time it admits them until.
type grantObservation struct {
	aspect DiffAspect
	// value is the fact as a report says it, normalised so that two documents spelling
	// the same fact differently produce the same string.
	value string
	// statement is the identifier of the statement it was read from, if it has one.
	statement string
}

// observeBucketAccess reads what a bucket policy grants over a dataset's objects.
//
// Only Allow statements that reach the data are read, and reaching the data means either
// fetching an object in the bucket or listing the bucket. A listing grant counts because
// the keys of a genomic dataset carry the sample identifiers a grant exists to control,
// so being able to list is a disclosure in itself.
//
// Deny statements are skipped. They take access away rather than granting it, which is
// exactly what the documents rendered here use them for, and a Deny that has been
// tampered with shows up as the grant it no longer restrains.
func observeBucketAccess(doc *policy.Policy, bucket string) []grantObservation {
	var observations []grantObservation
	for _, st := range statementsOf(doc) {
		if !strings.EqualFold(st.Effect, policy.EffectAllow) {
			continue
		}
		reads := iampolicy.ActionGrants(st.Action, "s3:GetObject") &&
			iampolicy.ResourceNamesObjectIn(st.Resource, bucket)
		lists := iampolicy.ActionGrants(st.Action, "s3:ListBucket") &&
			iampolicy.ResourceNamesBucket(st.Resource, bucket)
		if !reads && !lists {
			continue
		}
		observations = append(observations, observeStatement(st)...)
	}
	return observations
}

// observeKeyAccess reads what a key policy grants over decryption of a dataset's
// objects.
//
// Decryption is the only permission that bears on reading: bucket keys are off, so every
// read is decrypted per request and whoever may decrypt may read whatever they can
// address. The statements that administer the key or withdraw it grant no decryption and
// so state nothing about access. There is nothing to scope on either -- a key policy
// applies to its own key, and its resource element is always the wildcard.
func observeKeyAccess(doc *policy.Policy) []grantObservation {
	var observations []grantObservation
	for _, st := range statementsOf(doc) {
		if !strings.EqualFold(st.Effect, policy.EffectAllow) {
			continue
		}
		if !iampolicy.ActionGrants(st.Action, "kms:Decrypt") {
			continue
		}
		observations = append(observations, observeStatement(st)...)
	}
	return observations
}

// observeStatement reads the three facts one granting statement states.
func observeStatement(st policy.Statement) []grantObservation {
	sid := strings.TrimSpace(st.Sid)
	var observations []grantObservation
	observe := func(aspect DiffAspect, values ...string) {
		for _, value := range values {
			observations = append(observations, grantObservation{
				aspect:    aspect,
				value:     value,
				statement: sid,
			})
		}
	}

	principals := iampolicy.PrincipalsOf(st.Principal)
	if principals.All {
		observe(AspectGrantedAccount, everyPrincipal)
	}
	observe(AspectGrantedAccount, principals.Accounts...)
	// A principal naming no account is reported as it was written. That it grants
	// something is the point; deciding it grants nothing would be a guess.
	observe(AspectGrantedAccount, principals.Others...)

	observe(AspectPermittedVpc, orElse(iampolicy.ConditionOperands(st, sourceVpcConditionKey), anyVpc)...)
	observe(AspectExpiry, orElse(timeCeilings(st), noExpiry)...)

	return observations
}

// orElse returns values, or the single fallback when the statement said nothing on the
// subject.
func orElse(values []string, fallback string) []string {
	if len(values) == 0 {
		return []string{fallback}
	}
	return values
}

// timeCeilings reads the times a statement admits requests until, normalised to whole
// UTC seconds so that the same instant written with a different offset or a fractional
// second does not read as a different expiry. A value that is not a time is reported
// verbatim, since something has to be said about it and guessing at what was meant is
// worse.
func timeCeilings(st policy.Statement) []string {
	var ceilings []string
	for _, operator := range timeCeilingOperators {
		for _, operand := range iampolicy.ConditionOperandsFor(st, operator, requestTimeConditionKey) {
			operand = strings.TrimSpace(operand)
			if parsed, err := time.Parse(time.RFC3339, operand); err == nil {
				operand = policyTime(parsed)
			}
			ceilings = append(ceilings, operand)
		}
	}
	return ceilings
}

// statementsOf returns a document's statements, treating an absent document as one that
// says nothing.
func statementsOf(doc *policy.Policy) []policy.Statement {
	if doc == nil || doc.Statements == nil {
		return nil
	}
	return doc.Statements.Values()
}

// compareAccess reports how the facts a deployed document states differ from the facts
// the described document states, aspect by aspect.
func compareAccess(document ApplyTarget, expected, deployed []grantObservation) []PolicyDifference {
	var differences []PolicyDifference
	for _, aspect := range aspectOrder {
		want := factsOf(expected, aspect)
		got := factsOf(deployed, aspect)

		missing := factsOnlyIn(want, got)
		unexpected := factsOnlyIn(got, want)

		switch {
		case len(missing) == 0 && len(unexpected) == 0:
			continue
		case len(missing) == 1 && len(unexpected) == 1:
			// One value standing where another should: the same fact, changed. Reporting
			// it as a single difference carrying both sides is what makes "the expiry
			// moved" legible, rather than two findings that look unrelated.
			differences = append(differences, PolicyDifference{
				Document:  document,
				Aspect:    aspect,
				Expected:  missing[0],
				Deployed:  unexpected[0],
				Statement: got[unexpected[0]],
			})
		default:
			for _, value := range missing {
				differences = append(differences, PolicyDifference{
					Document: document,
					Aspect:   aspect,
					Expected: value,
				})
			}
			for _, value := range unexpected {
				differences = append(differences, PolicyDifference{
					Document:  document,
					Aspect:    aspect,
					Deployed:  value,
					Statement: got[value],
				})
			}
		}
	}
	return differences
}

// factsOf collects the distinct values observed for one aspect, each against the
// statement it was first seen in. A fact stated twice -- by two statements saying the
// same thing -- is one fact.
func factsOf(observations []grantObservation, aspect DiffAspect) map[string]string {
	facts := make(map[string]string)
	for _, observation := range observations {
		if observation.aspect != aspect {
			continue
		}
		if _, seen := facts[observation.value]; !seen {
			facts[observation.value] = observation.statement
		}
	}
	return facts
}

// factsOnlyIn returns the values present in one set and not the other, sorted so that a
// report of the same drift reads the same way every time.
func factsOnlyIn(these, those map[string]string) []string {
	var only []string
	for value := range these {
		if _, found := those[value]; !found {
			only = append(only, value)
		}
	}
	slices.Sort(only)
	return only
}
