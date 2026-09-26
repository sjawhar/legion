package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

type stubSMAPI struct {
	out *secretsmanager.GetSecretValueOutput
	err error
}

func (s stubSMAPI) GetSecretValue(_ context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return s.out, s.err
}

func TestAWSReadReturnsErrNotFoundOnResourceNotFoundException(t *testing.T) {
	r := AWS{Client: stubSMAPI{err: &types.ResourceNotFoundException{}}}
	_, err := r.Read(context.Background(), "arn:aws:secretsmanager:us-east-1:1:secret:missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAWSReadReturnsSecretStringOnSuccess(t *testing.T) {
	r := AWS{Client: stubSMAPI{out: &secretsmanager.GetSecretValueOutput{SecretString: aws.String("shh")}}}
	v, err := r.Read(context.Background(), "arn:aws:secretsmanager:us-east-1:1:secret:present")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "shh" {
		t.Fatalf("expected %q, got %q", "shh", v)
	}
}

func TestFakeReadReturnsErrNotFoundForMissingName(t *testing.T) {
	f := Fake{"present": "shh"}
	_, err := f.Read(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFakeReadReturnsValueForPresentName(t *testing.T) {
	f := Fake{"present": "shh"}
	v, err := f.Read(context.Background(), "present")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "shh" {
		t.Fatalf("expected %q, got %q", "shh", v)
	}
}
