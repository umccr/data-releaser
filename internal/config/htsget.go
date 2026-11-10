package config

// The sections in this file belong to the htsget mechanism, where reads are served
// through htsget-rs and every request is authorized by a callout against a stored
// record. Nothing here is meaningful to a release that hands a researcher direct read
// access to a copy of the data, which is why an S3 command declares none of it.

import (
	"fmt"
	"strings"
	"time"

	"github.com/umccr/data-releaser/internal/model"
)

// WarehouseConfig holds OrcaVault Mart (Athena/Lake Formation) settings.
type WarehouseConfig struct {
	AthenaWorkgroup string `yaml:"athenaWorkgroup"`
	Database        string `yaml:"database"`
	Catalog         string `yaml:"catalog"`
}

// EndpointsConfig holds service endpoint URLs.
type EndpointsConfig struct {
	Authorization string `yaml:"authorization"`
	HtsgetBaseURL string `yaml:"htsgetBaseUrl"`
	// APIGatewayRestAPIID is the REST API ID for the Authorization Service API Gateway.
	// Used by the `validate` command's API Gateway stage check.
	APIGatewayRestAPIID string `yaml:"apiGatewayRestApiId"`
	// APIGatewayStageName is the stage name for the Authorization Service API Gateway.
	// Used by the `validate` command's API Gateway stage check. Defaults to "prod".
	APIGatewayStageName string `yaml:"apiGatewayStageName"`
}

// CacheConfig holds cache TTL settings for the authorization response.
type CacheConfig struct {
	TTLCeiling Duration `yaml:"ttlCeiling"`
	DenyTTL    Duration `yaml:"denyTtl"`
}

// DefaultCacheTTLCeiling is the default max-age ceiling for authorization responses.
const DefaultCacheTTLCeiling = 1 * time.Hour

// DefaultCacheDenyTTL is the default negative-cache TTL for deny responses.
const DefaultCacheDenyTTL = 60 * time.Second

// ValidationConfig holds optional validation/canary settings.
type ValidationConfig struct {
	Canary *CanaryConfig `yaml:"canary"`

	// SkipHtsgetCheck disables the direct htsget-rs /service-info health check.
	//
	// Set this when htsget-rs is fronted by VPC Lattice: its domain only resolves from
	// inside an associated VPC, so the check fails from an operator's machine even
	// though the deployment is healthy. Verify htsget-rs from inside the consumer VPC
	// instead.
	SkipHtsgetCheck bool `yaml:"skipHtsgetCheck"`

	// SkipAuthorizationCheck disables the Authorization Service reachability probe.
	//
	// Set this when the authorization API is deployed as a private API Gateway
	// endpoint (the default): it is served by an interface endpoint inside the data
	// account VPC, so its hostname does not resolve from an operator's machine and
	// the resource policy denies anything arriving another way. The check would fail
	// on a healthy deployment. Verify the callout from inside that VPC instead.
	SkipAuthorizationCheck bool `yaml:"skipAuthorizationCheck"`
}

// CanaryConfig holds the canary dataset reference for test-setup mode.
type CanaryConfig struct {
	LibraryID string `yaml:"libraryId"`
	Account   string `yaml:"account"`
	Bucket    string `yaml:"bucket"`
	Key       string `yaml:"key"`
}

// ResearcherConfig holds the per-account settings for one researcher account.
type ResearcherConfig struct {
	// TargetVpcArn is the ARN of the researcher VPC that is associated with the
	// htsget VPC Lattice service network.
	//
	// This is the identity the authorization callout is keyed on, and it must be the
	// value htsget-rs actually forwards. VPC Lattice puts the *EC2 VPC* ARN of the
	// calling VPC on the invocation event
	// (`$.requestContext.identity.sourceVpcArn`), so the form is
	// "arn:aws:ec2:<region>:<account>:vpc/vpc-<id>" — not a vpc-lattice ARN. The
	// authorization lookup is an exact-match query on a DynamoDB GSI, so any other
	// value silently denies every request.
	//
	// The S3 mechanism takes a VPC in a different form — the bare "vpc-…" identifier,
	// compared literally against the VPC a request to S3 arrived from — and passes it
	// per release rather than holding it here.
	TargetVpcArn string `yaml:"targetVpcArn"`
}

// ResolveTargetVpcArn returns the configured target VPC ARN for a researcher account.
// It returns an error when the account has no entry, because there is no safe value to
// fall back to: a guessed ARN would never match a real request.
func (cfg *Config) ResolveTargetVpcArn(accountID string) (string, error) {
	researcher, ok := cfg.Researchers[accountID]
	if !ok || strings.TrimSpace(researcher.TargetVpcArn) == "" {
		return "", fmt.Errorf(
			"no target VPC ARN configured for researcher account %s; add it under "+
				"researchers.%s.targetVpcArn in the config file, or pass --target-vpc-arn. "+
				"It is the ARN of the researcher VPC associated with the htsget VPC Lattice "+
				"service network, e.g. arn:aws:ec2:ap-southeast-2:%s:vpc/vpc-0123456789abcdef0",
			accountID, accountID, accountID)
	}

	arn := strings.TrimSpace(researcher.TargetVpcArn)
	if err := model.ValidateTargetVpcArn(arn, accountID); err != nil {
		return "", fmt.Errorf("researchers.%s.targetVpcArn: %w", accountID, err)
	}
	return arn, nil
}
