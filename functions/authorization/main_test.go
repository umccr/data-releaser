package main

import (
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/umccr/data-releaser/internal/auth"
)

// The requester identity is the only input to an authorization decision, so how
// it is lifted off the proxy event decides whether every callout matches a share
// or none of them do. htsget-rs sends it as a header and API Gateway is free to
// change the casing on the way through, so these cases pin the lookup.
func TestSourceVpcArnFrom(t *testing.T) {
	const arn = "arn:aws:ec2:ap-southeast-2:534840902377:vpc/vpc-0abc123"

	tests := []struct {
		name string
		req  events.APIGatewayProxyRequest
		want string
	}{
		{
			name: "canonical header casing",
			req: events.APIGatewayProxyRequest{
				Headers: map[string]string{auth.SourceVpcArnHeader: arn},
			},
			want: arn,
		},
		{
			name: "lowercased by the gateway",
			req: events.APIGatewayProxyRequest{
				Headers: map[string]string{"htsget-context-sourcevpcarn": arn},
			},
			want: arn,
		},
		{
			name: "multi-value headers only",
			req: events.APIGatewayProxyRequest{
				MultiValueHeaders: map[string][]string{
					"Htsget-Context-Sourcevpcarn": {arn},
				},
			},
			want: arn,
		},
		{
			name: "query string fallback for out-of-band probes",
			req: events.APIGatewayProxyRequest{
				QueryStringParameters: map[string]string{sourceVpcArnParam: arn},
			},
			want: arn,
		},
		{
			name: "header wins over the query string",
			req: events.APIGatewayProxyRequest{
				Headers:               map[string]string{auth.SourceVpcArnHeader: arn},
				QueryStringParameters: map[string]string{sourceVpcArnParam: "arn:aws:ec2:ap-southeast-2:000000000000:vpc/vpc-other"},
			},
			want: arn,
		},
		{
			// Not an error: an unidentified caller matches no share and the
			// callout handler denies it.
			name: "absent everywhere",
			req:  events.APIGatewayProxyRequest{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sourceVpcArnFrom(tt.req); got != tt.want {
				t.Errorf("sourceVpcArnFrom() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The header name is the contract with htsget-rs: it is the `Htsget-Context-`
// prefix plus the `name` of the forwarded context extension configured in
// htsget-pipeline. Renaming one side without the other silently denies everyone.
func TestSourceVpcArnHeaderMatchesTheForwardedContextName(t *testing.T) {
	if auth.SourceVpcArnHeader != "Htsget-Context-SourceVpcArn" {
		t.Errorf("header is %q, but htsget-rs forwards the context extension named "+
			"SourceVpcArn as Htsget-Context-SourceVpcArn", auth.SourceVpcArnHeader)
	}
}
