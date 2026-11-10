package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ManifestConfig holds a manifest delivery target. The bucket is a single same-account
// bucket in the account this tool runs against; each researcher account's manifest lives
// under a per-account key prefix.
//
// Both mechanisms deliver through a bucket of this shape, and by default the same one:
// they differ in the key template, since the two documents describe different kinds of
// access and one researcher may hold both.
type ManifestConfig struct {
	// Bucket is the manifest bucket name.
	Bucket string `yaml:"bucket"`
	// KeyTemplate is the per-account object key template. It supports the
	// {accountId} placeholder.
	KeyTemplate string `yaml:"keyTemplate"`
}

// DefaultManifestKeyTemplate is the default per-account key template for the htsget
// manifest.
const DefaultManifestKeyTemplate = "manifests/{accountId}/manifest.json"

// DefaultDatasetManifestKeyTemplate is the default per-account key template for the
// document describing a researcher's direct-read dataset access. It sits under its own
// prefix so that the two manifests can share a bucket without one overwriting the other.
const DefaultDatasetManifestKeyTemplate = "datasets/{accountId}/manifest.json"

// manifestPlaceholderRe matches {...} placeholder tokens in a key template.
var manifestPlaceholderRe = regexp.MustCompile(`\{[^}]*\}`)

// RenderManifestKey renders the manifest key template for the given account ID by
// substituting the {accountId} placeholder. Any other {...} token is rejected with
// a descriptive error so misconfigured templates fail fast.
func (m ManifestConfig) RenderManifestKey(accountID string) (string, error) {
	var rerr error
	rendered := manifestPlaceholderRe.ReplaceAllStringFunc(m.KeyTemplate, func(token string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(token, "{"), "}")
		switch name {
		case "accountId":
			return accountID
		default:
			if rerr == nil {
				rerr = fmt.Errorf("unknown placeholder %q in manifest key template %q (only {accountId} is supported)", token, m.KeyTemplate)
			}
			return token
		}
	})
	if rerr != nil {
		return "", rerr
	}
	return rendered, nil
}

// RenderManifestPrefix renders the per-account manifest key prefix (the directory
// portion of the rendered key with a trailing "*"), for example
// "manifests/<accountId>/*". Used to describe the researcher's cross-account read scope.
func (m ManifestConfig) RenderManifestPrefix(accountID string) (string, error) {
	key, err := m.RenderManifestKey(accountID)
	if err != nil {
		return "", err
	}
	if idx := strings.LastIndex(key, "/"); idx >= 0 {
		return key[:idx+1] + "*", nil
	}
	return "*", nil
}
