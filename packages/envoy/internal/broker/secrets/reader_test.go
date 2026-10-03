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
		t.Fatalf("Read returned an unexpected value: got %q", strings.ReplaceAll(v, "shh", "[REDACTED]"))
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

// TestLocalFromFileServesTheFileAsSecretsManagerWould pins the development file's shape: each
// secret is listed under its name with its tags and key, filtered by name prefix as Secrets
// Manager filters, and read by name or by the ARN the listing gave it.
func TestLocalFromFileServesTheFileAsSecretsManagerWould(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	content := `{"secrets": [
		{"name": "dev/agent-secrets/demo-key", "kms_key_id": "alias/dev", "tags": {"owner": "shared", "tier": "agent"}, "value": "demo-v1"},
		{"name": "other/demo-key", "kms_key_id": "", "tags": {}, "value": "other-v1"}
	]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	local, err := LocalFromFile(path)
	if err != nil {
		t.Fatalf("LocalFromFile: %v", err)
	}
	ctx := context.Background()
	listed, err := local.ListSecrets(ctx, &secretsmanager.ListSecretsInput{
		Filters: []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{"DEV/agent-secrets/"}}},
	})
	if err != nil || len(listed.SecretList) != 1 {
		t.Fatalf("ListSecrets = %+v, %v; want only dev/agent-secrets/demo-key", listed, err)
	}
	entry := listed.SecretList[0]
	if aws.ToString(entry.Name) != "dev/agent-secrets/demo-key" || aws.ToString(entry.KmsKeyId) != "alias/dev" || len(entry.Tags) != 2 {
		t.Fatalf("listed entry = %+v, want its name, key and two tags", entry)
	}
	for _, id := range []string{"dev/agent-secrets/demo-key", aws.ToString(entry.ARN)} {
		v, err := AWS{Client: local}.Read(ctx, id)
		if err != nil || v != "demo-v1" {
			t.Fatalf("Read(%s) = %v, want the value", id, err)
		}
	}
	if _, err := (AWS{Client: local}).Read(ctx, "dev/agent-secrets/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(missing) = %v, want ErrNotFound", err)
	}
}

func TestLocalFromFileRejectsMissingFile(t *testing.T) {
	_, err := LocalFromFile(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// TestLocalFromFileNeverQuotesTheFile pins that a file that does not parse is refused naming the
// path and where it failed, never its content, which could itself be or contain a secret value.
func TestLocalFromFileNeverQuotesTheFile(t *testing.T) {
	const sensitive = "sk-should-never-appear-in-any-error-message"
	for name, content := range map[string]string{
		"not JSON":                  sensitive + "\n",
		"a value of the wrong type": `{"secrets": [{"name": "x", "value": 7}]} ` + sensitive,
		"an unknown field":          `{"secrets": [{"name": "x", "secret_value": "` + sensitive + `"}]}`,
		"truncated":                 `{"secrets": [{"name": "x", "value": "` + sensitive,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			_, err := LocalFromFile(path)
			if err == nil {
				t.Fatal("expected an error for a file that does not parse")
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q should name the path", strings.ReplaceAll(err.Error(), sensitive, "[REDACTED]"))
			}
			if strings.Contains(err.Error(), sensitive) {
				t.Fatalf("error %q must never quote the file's content", strings.ReplaceAll(err.Error(), sensitive, "[REDACTED]"))
			}
		})
	}
}
