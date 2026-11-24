module github.com/umccr/data-releaser

go 1.27.4

// The CDK app's node_modules contain .go file that should be ignored, and are not part of this module.
ignore infrastructure/node_modules

require (
	github.com/aws/aws-lambda-go v1.55.1
	github.com/aws/aws-sdk-go-v2 v1.48.0
	github.com/aws/aws-sdk-go-v2/config v1.33.21
	github.com/aws/aws-sdk-go-v2/credentials v1.21.3
	github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue v1.21.23
	github.com/aws/aws-sdk-go-v2/service/apigateway v1.53.0
	github.com/aws/aws-sdk-go-v2/service/athena v1.68.0
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.74.1
	github.com/aws/aws-sdk-go-v2/service/kms v1.63.0
	github.com/aws/aws-sdk-go-v2/service/lambda v1.118.0
	github.com/aws/aws-sdk-go-v2/service/ram v1.46.1
	github.com/aws/aws-sdk-go-v2/service/s3 v1.116.0
	github.com/aws/aws-sdk-go-v2/service/sfn v1.53.0
	github.com/aws/aws-sdk-go-v2/service/sts v1.53.0
	github.com/micahhausler/aws-iam-policy v0.4.4
	github.com/oklog/ulid/v2 v2.1.2
	github.com/spf13/cobra v1.10.2
	github.com/testcontainers/testcontainers-go v0.45.0
	gopkg.in/yaml.v3 v3.0.1
	pgregory.net/rapid v1.3.0
)

require (
	dario.cat/mergo v1.0.2 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20261013165250-51075c6ea6bc // indirect
	github.com/Microsoft/go-winio v0.6.4 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.27 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.9 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/dynamodbstreams v1.48.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.25 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.13.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.14.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.5 // indirect
	github.com/aws/smithy-go v1.28.9 // indirect
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.2.0 // indirect
	github.com/containerd/platforms v0.2.1 // indirect
	github.com/cpuguy83/dockercfg v0.3.2 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-connections v0.8.2 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/ebitengine/purego v0.11.3 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/klauspost/compress v1.20.5 // indirect
	github.com/lufia/plan9stats v0.0.0-20261101121723-be84dd9509a0 // indirect
	github.com/magiconair/properties v1.18.13 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/go-archive v0.3.3 // indirect
	github.com/moby/moby/api v1.57.0 // indirect
	github.com/moby/moby/client v0.6.3 // indirect
	github.com/moby/patternmatcher v0.6.1 // indirect
	github.com/moby/sys/sequential v0.7.0 // indirect
	github.com/moby/sys/user v0.5.0 // indirect
	github.com/moby/sys/userns v0.2.1 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/power-devops/perfstat v0.0.0-20260916203055-22a1a467d9f0 // indirect
	github.com/shirou/gopsutil/v4 v4.26.10 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	github.com/spf13/pflag v1.1.0 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/tklauser/go-sysconf v0.4.0 // indirect
	github.com/tklauser/numcpus v0.12.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.2 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/crypto v0.59.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
)
