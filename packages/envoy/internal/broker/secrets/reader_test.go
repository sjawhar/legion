package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		t.Fatalf("Read returned an unexpected value (match = %v)", v == "shh")
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
		t.Fatalf("error message must not contain a secret value, got %q", strings.ReplaceAll(err.Error(), "shh", "[REDACTED]"))
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
		t.Fatalf("Read returned an unexpected value (match = %v)", v == "shh")
	}
}

func TestFakeFromFileParsesNameValueLinesSkippingCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	content := "# a comment\n\nDEEL_API_KEY=deel-v1\nAUTO_TOKEN=auto-v1\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	fake, err := FakeFromFile(path)
	if err != nil {
		t.Fatalf("FakeFromFile: %v", err)
	}
	if len(fake) != 2 || fake["DEEL_API_KEY"] != "deel-v1" || fake["AUTO_TOKEN"] != "auto-v1" {
		t.Fatalf("fake has %d entries (want 2); DEEL_API_KEY matches = %v, AUTO_TOKEN matches = %v", len(fake), fake["DEEL_API_KEY"] == "deel-v1", fake["AUTO_TOKEN"] == "auto-v1")
	}
}

func TestFakeFromFileRejectsMissingFile(t *testing.T) {
	_, err := FakeFromFile(filepath.Join(t.TempDir(), "missing.env"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// TestFakeFromFileRejectsLineWithNoEquals is also the regression for the review's Important
// finding: a malformed line's own content — which could itself be or contain a secret value —
// must never appear in the error, only the path and the line number.
func TestFakeFromFileRejectsLineWithNoEquals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	const sensitive = "sk-should-never-appear-in-any-error-message"
	if err := os.WriteFile(path, []byte(sensitive+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := FakeFromFile(path)
	if err == nil {
		t.Fatal("expected an error for a malformed line")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "1") {
		t.Fatalf("error %q should name the path and line number", err.Error())
	}
	if strings.Contains(err.Error(), sensitive) {
		t.Fatalf("error %q must never quote the malformed line's own content", err.Error())
	}
}
