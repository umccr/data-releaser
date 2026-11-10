// Package iampolicy reads what an AWS resource policy document grants.
//
// A document written here and the same document read back from AWS are not the same
// text. A one-element list comes back as a bare string, a principal written as an
// account root ARN may come back as the bare account id, statements come back in any
// order, and a person editing the document in a console is free to respell any of it
// again. So anything that inspects a deployed document has to ask what it grants rather
// than how it is written, and this package is where the spelling is normalised away.
//
// Nothing here evaluates a request. A grant these helpers find can still be overridden
// by an explicit Deny elsewhere in the same document, or by the caller's own
// permissions, so a positive answer means the document contains that grant -- never
// that the access is certain.
package iampolicy

import (
	"encoding/json"
	"slices"
	"strings"

	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/micahhausler/aws-iam-policy/policy"
)

const (
	// accountIDLength is the number of digits in an AWS account id.
	accountIDLength = 12

	// allResources is the resource element that names everything a document could apply
	// to.
	allResources = "*"
)

// Principals is the set of principals one statement addresses, sorted into the forms a
// caller can act on.
type Principals struct {
	// Accounts holds the AWS accounts named, whether they were written as a bare account
	// id or as an ARN belonging to that account.
	Accounts []string

	// Others holds the principals that name no account, verbatim. They are kept rather
	// than dropped so that a caller reporting on a document can say what it found instead
	// of quietly ignoring a grant it did not recognise.
	Others []string

	// All reports a statement addressed to every principal.
	All bool
}

// PrincipalsOf reads the AWS principals of a statement.
//
// Only AWS principals are read; a service or federated principal names no account and
// is not reported. The bare-string form needs care: the policy library reports both "*"
// and "arn:aws:iam::<account>:root" -- which AWS accepts as shorthand -- with the kind
// "All", so the two have to be told apart by value rather than by kind.
func PrincipalsOf(p *policy.Principal) Principals {
	if p == nil {
		return Principals{}
	}
	if awsPrincipal := p.AWS(); awsPrincipal != nil {
		return principalsOf(awsPrincipal.Values())
	}

	// No AWS key, so this is either the bare-string form or a kind that names no account.
	// Marshalling round-trips the original string, quoted; every other form marshals as an
	// object and so does not unmarshal into a string.
	raw, err := p.MarshalJSON()
	if err != nil {
		return Principals{}
	}
	var bare string
	if err := json.Unmarshal(raw, &bare); err != nil {
		return Principals{}
	}
	return principalsOf([]string{bare})
}

// principalsOf sorts principal strings into accounts, everything else, and the
// wildcard.
func principalsOf(values []string) Principals {
	var principals Principals
	for _, value := range values {
		value = strings.TrimSpace(value)
		switch {
		case value == "":
		case value == policy.PrincipalAll:
			principals.All = true
		case isAccountID(value):
			principals.Accounts = append(principals.Accounts, value)
		default:
			// An ARN's account field, rather than a substring search: an unrelated ARN that
			// merely contains the digits somewhere -- in a role path, say -- names a different
			// account entirely.
			if parsed, err := awsarn.Parse(value); err == nil && parsed.AccountID != "" {
				principals.Accounts = append(principals.Accounts, parsed.AccountID)
				continue
			}
			principals.Others = append(principals.Others, value)
		}
	}
	return principals
}

// IncludesAccount reports whether these principals reach the given account, either by
// naming it or by addressing everyone.
func (p Principals) IncludesAccount(accountID string) bool {
	return p.All || slices.Contains(p.Accounts, strings.TrimSpace(accountID))
}

// isAccountID reports whether s is an AWS account id: exactly twelve digits.
func isAccountID(s string) bool {
	if len(s) != accountIDLength {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ActionGrants reports whether an action list covers the given action.
//
// A trailing wildcard is honoured, so "s3:*" and "*" both cover "s3:GetObject", and
// matching is case-insensitive as IAM action names are. A wildcard anywhere other than
// at the end is not interpreted: no document written here uses one, and a grant check
// that guesses is worse than one that reports the grant as absent.
func ActionGrants(actions *policy.StringOrSlice, action string) bool {
	if actions == nil {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(action))
	for _, candidate := range actions.Values() {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if pattern, found := strings.CutSuffix(candidate, "*"); found {
			if strings.HasPrefix(want, pattern) {
				return true
			}
			continue
		}
		if candidate == want {
			return true
		}
	}
	return false
}

// ResourceCoversPrefix reports whether a resource list grants access to every object
// under prefixPath in bucket.
//
// Each resource is an S3 object ARN, optionally ending in a wildcard. Matching is done
// on the parsed ARN's resource path ("<bucket>/<key>") rather than by substring, so a
// grant on a different bucket, or on a sibling prefix, is not mistaken for one on this
// path.
func ResourceCoversPrefix(resources *policy.StringOrSlice, bucket, prefixPath string) bool {
	paths, all := s3ResourcePaths(resources)
	if all {
		return true
	}
	want := bucket + "/" + prefixPath
	for _, path := range paths {
		// A trailing wildcard grants everything below the path it follows; anything else must
		// name the prefix exactly.
		if pattern, found := strings.CutSuffix(path, "*"); found {
			if strings.HasPrefix(want, pattern) {
				return true
			}
			continue
		}
		if path == want {
			return true
		}
	}
	return false
}

// ResourceNamesObjectIn reports whether a resource list names any object in bucket,
// which is the resource the object-level actions apply to.
//
// It differs from ResourceCoversPrefix in asking whether the list reaches the objects
// at all rather than whether it reaches all of them: a grant scoped to one prefix of a
// bucket is still a grant over that bucket's data.
func ResourceNamesObjectIn(resources *policy.StringOrSlice, bucket string) bool {
	paths, all := s3ResourcePaths(resources)
	if all {
		return true
	}
	objects := bucket + "/"
	for _, path := range paths {
		if pattern, found := strings.CutSuffix(path, "*"); found {
			// The wildcard reaches an object either when its fixed part is inside the bucket, or
			// when it stops short of the bucket's own name and so subsumes it.
			if strings.HasPrefix(pattern, objects) || strings.HasPrefix(objects, pattern) {
				return true
			}
			continue
		}
		if strings.HasPrefix(path, objects) && len(path) > len(objects) {
			return true
		}
	}
	return false
}

// ResourceNamesBucket reports whether a resource list names the bucket itself, which is
// the resource the listing actions apply to rather than the objects.
func ResourceNamesBucket(resources *policy.StringOrSlice, bucket string) bool {
	paths, all := s3ResourcePaths(resources)
	if all {
		return true
	}
	for _, path := range paths {
		if pattern, found := strings.CutSuffix(path, "*"); found {
			if strings.HasPrefix(bucket, pattern) {
				return true
			}
			continue
		}
		if path == bucket {
			return true
		}
	}
	return false
}

// s3ResourcePaths returns the resource paths an S3 resource list names -- "<bucket>" or
// "<bucket>/<key>", either exact or ending in a wildcard -- and whether the list names
// every resource there is.
//
// Resources that are not S3 ARNs are dropped: they say nothing about access to a
// bucket, and a raw string comparison against them is what turns an unrelated ARN
// containing a bucket's name into an apparent grant.
func s3ResourcePaths(resources *policy.StringOrSlice) (paths []string, all bool) {
	if resources == nil {
		return nil, false
	}
	for _, candidate := range resources.Values() {
		candidate = strings.TrimSpace(candidate)
		if candidate == allResources {
			all = true
			continue
		}
		parsed, err := awsarn.Parse(candidate)
		if err != nil || parsed.Service != "s3" {
			continue
		}
		paths = append(paths, parsed.Resource)
	}
	return paths, all
}

// ConditionOperands returns the string operands a statement conditions the given
// condition key on, under whichever operators it uses, sorted so that repeated reads of
// one document report them in the same order.
//
// Only string operands are returned; a numeric or boolean condition bounds nothing that
// is expressed as a string.
func ConditionOperands(st policy.Statement, conditionKey string) []string {
	var operands []string
	for operator := range st.Condition {
		operands = append(operands, ConditionOperandsFor(st, operator, conditionKey)...)
	}
	slices.Sort(operands)
	return operands
}

// ConditionOperandsFor returns the string operands a statement conditions the given
// condition key on under one named operator, so that a caller can tell, for instance,
// an upper bound on a date from a lower one.
//
// Both the operator and the condition key are matched case-insensitively. IAM compares
// condition keys that way, and a deployed document is worth reading leniently.
func ConditionOperandsFor(st policy.Statement, operator, conditionKey string) []string {
	var operands []string
	for candidateOperator, keys := range st.Condition {
		if !strings.EqualFold(candidateOperator, operator) {
			continue
		}
		for key, value := range keys {
			if value == nil || !strings.EqualFold(key, conditionKey) {
				continue
			}
			stringOperands, _, _ := value.Values()
			operands = append(operands, stringOperands...)
		}
	}
	return operands
}
