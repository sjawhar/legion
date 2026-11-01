package main

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestRDSAuthTokensSignsAConnectRequestForTheEndpointAndUser pins the minter the broker passes
// store.Open in IAM mode: an RDS IAM auth token is a SigV4-presigned rds-db connect request for
// the endpoint and database user, good for 900 seconds, signed with the AWS config's region and
// credentials; a config with no region is refused at startup rather than minting tokens RDS
// would refuse at every sign-in.
func TestRDSAuthTokensSignsAConnectRequestForTheEndpointAndUser(t *testing.T) {
	creds := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}, nil
	})
	mint, err := rdsAuthTokens(aws.Config{Region: "us-west-2", Credentials: creds})
	if err != nil {
		t.Fatalf("rdsAuthTokens: %v", err)
	}
	const endpoint = "example-cluster.cluster-abcdefghijkl.us-west-2.rds.amazonaws.com:5432"
	token, err := mint(context.Background(), endpoint, "agent_secrets_broker")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	signed, err := url.Parse("https://" + token)
	if err != nil {
		t.Fatalf("token %q is not a presigned request: %v", token, err)
	}
	query := signed.Query()
	for key, want := range map[string]string{"Action": "connect", "DBUser": "agent_secrets_broker", "X-Amz-Expires": "900"} {
		if got := query.Get(key); got != want {
			t.Errorf("token %s = %q, want %q", key, got, want)
		}
	}
	if signed.Host != endpoint {
		t.Errorf("token signs a request to %q, want %q", signed.Host, endpoint)
	}
	if got := query.Get("X-Amz-Credential"); !strings.Contains(got, "/us-west-2/rds-db/aws4_request") {
		t.Errorf("token's credential scope %q is not rds-db in us-west-2", got)
	}

	if _, err := rdsAuthTokens(aws.Config{Credentials: creds}); err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("rdsAuthTokens with no region = %v, want a refusal naming the region", err)
	}
}
