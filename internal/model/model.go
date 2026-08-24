// Package model defines the core domain types used across the Data Releaser,
// including ShareRecord, AccessRule, Manifest, and ResolvedLibrary.
package model

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"
)

// ShareStatus represents the effective status of a share record.
type ShareStatus string

const (
	ShareStatusActive  ShareStatus = "active"
	ShareStatusExpired ShareStatus = "expired"
	ShareStatusRevoked ShareStatus = "revoked"
)

// ShareRecord represents a persistent record describing which library IDs are shared
// to which AWS account, including expiry and status. Stored in DynamoDB with shareId
// as the partition key (Req 10.2).
type ShareRecord struct {
	// ShareID is the unique identifier for the share (ULID).
	ShareID string `json:"shareId"`

	// ResearcherAccountID is the target AWS account receiving the share.
	ResearcherAccountID string `json:"researcherAccountId"`

	// TargetVpcArn is the VPC ARN of the researcher's VPC Lattice connection.
	TargetVpcArn string `json:"targetVpcArn"`

	// LibraryIDs is the set of library identifiers included in this share.
	LibraryIDs []string `json:"libraryIds"`

	// S3Prefixes is the set of resolved S3 prefix locations for htsget access.
	// These are what the authorization callout grants on; they are directories, so
	// they cover co-located index files.
	S3Prefixes []string `json:"s3Prefixes"`

	// Files is the set of individual files resolved at share time, recorded so the
	// manifest can name each file's exact htsget request path. It is descriptive
	// only — authorization is granted by S3Prefixes, never by this field. Records
	// written before this field existed have none.
	Files []ShareFile `json:"files,omitempty"`

	// CreatedAt is the UTC timestamp when the share was created.
	CreatedAt time.Time `json:"createdAt"`

	// ExpiresAt is the optional expiry timestamp. Nil means unlimited duration (Req 2).
	ExpiresAt *time.Time `json:"expiresAt"`

	// RevokedAt is the optional timestamp when the share was revoked. Nil means not revoked.
	RevokedAt *time.Time `json:"revokedAt"`
}

// ShareFile is a single file covered by a share, as resolved from the warehouse at
// share time.
type ShareFile struct {
	// LibraryID is the library this file belongs to.
	LibraryID string `json:"libraryId"`

	// HtsgetID is the htsget resource id for the file: the S3 key without its bucket
	// and without the format extension, e.g. "demo/L2400001/NA12878". Prefixing it
	// with an endpoint gives the request path, e.g. "reads/demo/L2400001/NA12878".
	HtsgetID string `json:"htsgetId"`

	// Format is the lower-cased file format, e.g. "bam".
	Format string `json:"format"`
}

// EffectiveStatus computes the current status of the share record based on the
// current time. The derivation logic is:
//   - If revokedAt is set → "revoked"
//   - If expiresAt is set and <= now → "expired"
//   - Otherwise → "active"
func (s *ShareRecord) EffectiveStatus(now time.Time) ShareStatus {
	if s.RevokedAt != nil {
		return ShareStatusRevoked
	}
	if s.ExpiresAt != nil && !s.ExpiresAt.After(now) {
		return ShareStatusExpired
	}
	return ShareStatusActive
}

// AccessRule describes one allowed access scope for a file, mirroring the htsget-rs
// ReferenceNameRestriction. It is shared by both the Authorization_Service callout
// response (htsgetAuth[].rules) and the Manifest (files[].rules).
//
// Semantics: an absent/nil field means that dimension is unrestricted (allow-all).
// Multiple AccessRules for the same file combine as a union. An absent or empty
// rules array means unrestricted access.
type AccessRule struct {
	// ReferenceName restricts to a specific reference. Nil means all reference names.
	ReferenceName *string `json:"referenceName,omitempty"`

	// Format restricts to a specific format (BAM, CRAM, VCF, BCF). Nil means all formats.
	Format *string `json:"format,omitempty"`

	// Start is the 0-based inclusive start position. Nil means from the beginning.
	Start *int64 `json:"start,omitempty"`

	// End is the 0-based exclusive end position. Nil means to the end.
	End *int64 `json:"end,omitempty"`
}

// ManifestFile represents a single file entry within a manifest library.
type ManifestFile struct {
	// Request is the htsget request path relative to the base URL (e.g. "reads/<id>").
	Request string `json:"request"`

	// Rules is an optional array of AccessRules. Absent or empty means unrestricted access.
	Rules []AccessRule `json:"rules,omitempty"`
}

// ManifestLibrary represents a library entry within the manifest.
type ManifestLibrary struct {
	// LibraryID is the unique identifier for this library.
	LibraryID string `json:"libraryId"`

	// ExpiresAt is the effective expiry for this library (latest across shares, or nil if unlimited).
	ExpiresAt *time.Time `json:"expiresAt"`

	// Files is the list of accessible files for this library.
	Files []ManifestFile `json:"files"`
}

// Manifest is the JSON document listing the library IDs and file references that a
// researcher has been granted access to. One manifest per Researcher_Account (Req 7).
// The manifest is tool-generated and descriptive only — it does not grant access (Req 7.8).
type Manifest struct {
	// HtsgetBaseUrl is the htsget-rs base URL for constructing request URLs.
	HtsgetBaseUrl string `json:"htsgetBaseUrl"`

	// GeneratedAt is the ISO 8601 UTC timestamp when the manifest was generated.
	GeneratedAt time.Time `json:"generatedAt"`

	// Libraries is the list of library entries the researcher can access.
	Libraries []ManifestLibrary `json:"libraries"`
}

// ResolvedLibrary represents a library resolved from the OrcaVault Mart (mart.bam)
// via Athena. Each row corresponds to a single aligned-read file.
type ResolvedLibrary struct {
	// LibraryID is the unique library identifier.
	LibraryID string `json:"libraryId"`

	// PortalRunID is the workflow run identifier that produced this file.
	PortalRunID string `json:"portalRunId"`

	// Bucket is the S3 bucket where the file is stored.
	Bucket string `json:"bucket"`

	// Key is the S3 object key of the file.
	Key string `json:"key"`

	// Format is the file format (e.g. "bam").
	Format string `json:"format"`

	// StorageClass is the S3 storage class (e.g. "STANDARD", "DEEP_ARCHIVE").
	StorageClass string `json:"storageClass"`
}

// vpcArnRe matches an EC2 VPC ARN, the form VPC Lattice reports as the source of an
// invocation and therefore the only form that can match a forwarded identity.
var vpcArnRe = regexp.MustCompile(`^arn:aws[a-z-]*:ec2:[a-z0-9-]+:\d{12}:vpc/vpc-[0-9a-f]+$`)

// ErrInvalidTargetVpcArn is the sentinel every ValidateTargetVpcArn failure wraps, so
// callers can tell a rejected ARN (the caller's mistake) from a genuine failure to write
// the share.
var ErrInvalidTargetVpcArn = errors.New("invalid target VPC ARN")

// The two failure messages below are worded to read correctly after the sentinel, which
// fmt places first: "invalid target VPC ARN: <detail>".

// ValidateTargetVpcArn checks that arn is a well-formed EC2 VPC ARN and, when
// accountID is non-empty, that it belongs to that account. Every failure wraps
// ErrInvalidTargetVpcArn.
//
// Catching this matters because a malformed ARN produces no error downstream — it is
// simply written to the store and then never matches an authorization callout, so the
// mistake surfaces much later as an unexplained deny.
func ValidateTargetVpcArn(arn, accountID string) error {
	if !vpcArnRe.MatchString(arn) {
		return fmt.Errorf(
			"%w: %q is not a valid EC2 VPC ARN; expected the form "+
				"arn:aws:ec2:<region>:<account-id>:vpc/vpc-<id>, which is what VPC Lattice "+
				"reports as the source of a request", ErrInvalidTargetVpcArn, arn)
	}
	if accountID != "" {
		// Safe to ignore the parse error: vpcArnRe already matched, so the ARN is
		// well-formed.
		parsed, _ := awsarn.Parse(arn)
		if parsed.AccountID != accountID {
			return fmt.Errorf(
				"%w: %q belongs to account %s but the share targets account %s",
				ErrInvalidTargetVpcArn, arn, parsed.AccountID, accountID)
		}
	}
	return nil
}
