package config

// The section in this file belongs to the mechanism that copies a dataset into this
// account and grants a researcher direct read access to that copy. Access is enforced by
// the cache bucket's policy and by the policy of the key encrypting it, so nothing here
// is consulted at request time — but a wrong value produces an access denial or an
// unintended grant that only surfaces much later, which is why most of it is checked
// when a command declares it rather than when it is first used.

import (
	"fmt"
	"os"
	"strings"
)

// BucketNameHashKeyEnv names the environment variable carrying the secret that keys the
// derivation of cache bucket names from dataset URIs.
//
// It is not a config field. The config file is checked in alongside the deployment's
// context, and a checked-in key obscures nothing — anyone holding it can reproduce a
// name from a guessed dataset URI. The same variable keys the derivation on the
// deployment side, and it has to carry the same value in both places or the two derive
// different names for one dataset.
const BucketNameHashKeyEnv = "HOT_CACHE_BUCKET_NAME_HASH_KEY"

// DefaultHotCacheNamePrefix is the prefix applied to derived cache bucket names. It
// matches the deployment's default name prefix.
const DefaultHotCacheNamePrefix = "data-releaser"

// S3Config holds everything the direct-read mechanism needs.
type S3Config struct {
	// Copy locates the copy orchestration that moves a dataset out of the source store.
	Copy CopyServiceConfig `yaml:"copy"`

	// Source describes the external object store holding the authoritative copy.
	Source SourceStoreConfig `yaml:"source"`

	// HotCache describes how a dataset's cache bucket is named.
	HotCache HotCacheConfig `yaml:"hotCache"`

	// Manifest is where the document describing a researcher's dataset access is
	// delivered. Its bucket defaults to the shared manifest bucket.
	Manifest ManifestConfig `yaml:"manifest"`

	// CustodianRoleArn is the principal permitted to withdraw a dataset by disabling its
	// key or scheduling the key's deletion. Optional: where no separate custodian exists
	// the key policy omits the statement and withdrawal stays reachable through the
	// account's own key administration.
	CustodianRoleArn string `yaml:"custodianRoleArn"`
}

// CopyServiceConfig locates the copy orchestration and the role it writes through.
//
// The orchestration is deployed separately and consumed here, so all of this is
// configuration: a copy service installed in another account is a config change rather
// than a code change.
type CopyServiceConfig struct {
	// StateMachineArn identifies the state machine a copy is started on.
	StateMachineArn string `yaml:"stateMachineArn"`

	// Account is the account the copy orchestration is installed in. Empty means the
	// same account as the cache buckets. It is separate because a copy service is
	// normally installed to copy objects out of its own account and this mechanism
	// copies in, so the two may well not coincide.
	Account string `yaml:"account"`

	// WorkingBucket is the bucket the orchestration reads its instructions from and
	// writes its own outputs to. It belongs to the orchestration's installation, not to
	// this one.
	WorkingBucket string `yaml:"workingBucket"`

	// WorkingBucketPrefix is the prefix within that bucket the orchestration was
	// deployed with. Every key computed for it sits beneath this.
	WorkingBucketPrefix string `yaml:"workingBucketPrefix"`

	// WriterRoleName is the role the orchestration writes objects through. Each
	// dataset's key policy has to name it, or a copy fails partway through a transfer
	// that may already have run for hours. The orchestration fixes the name at
	// deployment time.
	WriterRoleName string `yaml:"writerRoleName"`

	// WriterRoleAccount owns that role. Empty means the same account as the cache
	// buckets.
	WriterRoleAccount string `yaml:"writerRoleAccount"`

	// Performance tunes how hard a copy is allowed to run against the source store.
	// Zero fields leave the orchestration's own defaults in place.
	//
	// It is configured here rather than fixed when the orchestration is deployed
	// because the right values follow the source, and one installation may copy from
	// several. The defaults suit a source that scales with the load put on it; a
	// source on a fixed link is saturated by them, and the resulting failure appears
	// in the small-object copies rather than the large transfers that caused it.
	Performance CopyPerformanceConfig `yaml:"performance"`
}

// CopyPerformanceConfig mirrors the orchestration's own performance settings. The names
// are its, deliberately, so that an operator reading its documentation finds the same
// words here.
type CopyPerformanceConfig struct {
	HeadConcurrency      int `yaml:"headConcurrency"`
	SmallCopyConcurrency int `yaml:"smallCopyConcurrency"`
	SmallCopyBatchSize   int `yaml:"smallCopyBatchSize"`
	LargeCopyConcurrency int `yaml:"largeCopyConcurrency"`
}

// SourceStoreConfig describes the external, S3-compatible object store holding the
// authoritative copy of every dataset.
type SourceStoreConfig struct {
	// EndpointURL is the store's custom S3 endpoint. Whether it is routable from the
	// subnets the copy orchestration runs in is a property of the deployment; only the
	// shape of the value is checked here.
	EndpointURL string `yaml:"endpointUrl"`

	// Region is the region the store presents itself as being in. S3-compatible stores
	// generally require some value even where the concept does not apply.
	Region string `yaml:"region"`

	// CredentialsSecret is the Secrets Manager secret holding the store's access key.
	// Only the identifier appears here and only the identifier travels: the copy
	// orchestration resolves the secret with its own permissions, so no source
	// credential passes through this tool.
	CredentialsSecret string `yaml:"credentialsSecret"`
}

// HotCacheConfig describes how a dataset's cache bucket is named.
//
// The bucket and key for a dataset are deployed out of band, one stack per dataset, and
// this tool only locates them — so it has to derive the same name the deployment did,
// from the same inputs.
type HotCacheConfig struct {
	// NamePrefix is the prefix applied to derived bucket names. It has to match the
	// prefix the deployment used, since a different prefix derives a different name and
	// therefore a different bucket.
	NamePrefix string `yaml:"namePrefix"`

	// ObscureBucketNames derives names with a keyed hash rather than from a legible
	// slug of the dataset URI, so a bucket name discloses nothing about what it holds.
	// The key comes from the environment; see BucketNameHashKeyEnv.
	ObscureBucketNames bool `yaml:"obscureBucketNames"`

	// Buckets maps a dataset URI to an explicit bucket name, bypassing derivation.
	// Worth setting once a bucket holds a copy someone is reading: derivation depends on
	// inputs that may change, and a changed name is a different bucket.
	Buckets map[string]string `yaml:"buckets"`
}

// ResolveBucketNameHashKey returns the key that derives obscured bucket names, or the
// empty string when names are derived legibly.
//
// Refusing to continue without a key is the point of the flag. Were the key merely
// treated as absent, names would silently fall back to their legible form — a different
// name, so a different bucket — and this tool would look for a released dataset in a
// bucket that holds nothing.
func (h HotCacheConfig) ResolveBucketNameHashKey() (string, error) {
	if !h.ObscureBucketNames {
		return "", nil
	}
	hashKey := strings.TrimSpace(os.Getenv(BucketNameHashKeyEnv))
	if hashKey == "" {
		return "", fmt.Errorf(
			"s3.hotCache.obscureBucketNames is set, so %s must carry the key cache bucket names are "+
				"derived with. It has to be the same value the deployment was synthesised with, or "+
				"the derived name points at a bucket that holds nothing",
			BucketNameHashKeyEnv)
	}
	return hashKey, nil
}
