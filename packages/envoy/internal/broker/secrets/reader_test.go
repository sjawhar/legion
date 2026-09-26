package secrets

import (
	"context"
	"errors"
	"strings"
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

func TestAWSReadWrapsGenericErrorInsteadOfMappingToErrNotFound(t *testing.T) {
	boom := errors.New("boom")
	r := AWS{Client: stubSMAPI{err: boom}}
	source := "arn:aws:secretsmanager:us-east-1:1:secret:present"
	_, err := r.Read(context.Background(), source)
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a generic error must not be mapped to ErrNotFound, got %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected the generic error to be wrapped, got %v", err)
	}
	if !strings.Contains(err.Error(), source) {
		t.Fatalf("expected the error to name the source %q, got %q", source, err.Error())
	}
	if strings.Contains(err.Error(), "shh") {
		t.Fatalf("error message must not contain a secret value, got %q", err.Error())
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
