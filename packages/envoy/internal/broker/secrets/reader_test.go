package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
// secret is listed under its name with its tags, key and version stages, filtered by name prefix as
// Secrets Manager filters, case-sensitively, and read by name or by the ARN the listing gave it. A
// secret the file gives no value is one created without a value: listed with no version, its read
// is ErrNotFound, until a value is put, which gives it an AWSCURRENT version.
func TestLocalFromFileServesTheFileAsSecretsManagerWould(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	content := `{"secrets": [
		{"name": "dev/agent-secrets/demo-key", "kms_key_id": "alias/dev", "tags": {"owner": "shared", "tier": "agent"}, "value": "demo-v1"},
		{"name": "dev/agent-secrets/unseeded-key", "kms_key_id": "alias/dev", "tags": {"owner": "shared", "tier": "agent"}},
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
	listByPrefix := func(prefix string) []types.SecretListEntry {
		listed, err := local.ListSecrets(ctx, &secretsmanager.ListSecretsInput{
			Filters: []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{prefix}}},
		})
		if err != nil {
			t.Fatalf("ListSecrets(%s): %v", prefix, err)
		}
		return listed.SecretList
	}
	if listed := listByPrefix("DEV/agent-secrets/"); len(listed) != 0 {
		t.Fatalf("ListSecrets(DEV/agent-secrets/) = %+v; want nothing, as Secrets Manager's name filter is case-sensitive", listed)
	}
	listed := listByPrefix("dev/agent-secrets/demo-key")
	if len(listed) != 1 {
		t.Fatalf("ListSecrets(dev/agent-secrets/demo-key) = %+v; want only dev/agent-secrets/demo-key", listed)
	}
	entry := listed[0]
	if aws.ToString(entry.Name) != "dev/agent-secrets/demo-key" || aws.ToString(entry.KmsKeyId) != "alias/dev" || len(entry.Tags) != 2 {
		t.Fatalf("listed entry = %+v, want its name, key and two tags", entry)
	}
	if stages := currentStages(entry); stages != 1 {
		t.Fatalf("demo-key lists %d AWSCURRENT versions in %v; want one", stages, entry.SecretVersionsToStages)
	}
	if listed := listByPrefix("dev/agent-secrets/"); len(listed) != 2 {
		t.Fatalf("ListSecrets(dev/agent-secrets/) = %+v; want demo-key and unseeded-key", listed)
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

	unseeded := listByPrefix("dev/agent-secrets/unseeded-key")
	if len(unseeded) != 1 || len(unseeded[0].SecretVersionsToStages) != 0 {
		t.Fatalf("ListSecrets(unseeded-key) = %+v; want it listed with no version", unseeded)
	}
	if _, err := (AWS{Client: local}).Read(ctx, "dev/agent-secrets/unseeded-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(unseeded-key) = %v, want ErrNotFound", err)
	}
	local.Put(LocalSecret{Name: "dev/agent-secrets/unseeded-key", KmsKeyID: "alias/dev", Tags: map[string]string{"owner": "shared", "tier": "agent"}, Value: "seeded-v1"})
	if seeded := listByPrefix("dev/agent-secrets/unseeded-key"); len(seeded) != 1 || currentStages(seeded[0]) != 1 {
		t.Fatalf("ListSecrets(unseeded-key) after its value was put = %+v; want one AWSCURRENT version", seeded)
	}
	if v, err := (AWS{Client: local}).Read(ctx, "dev/agent-secrets/unseeded-key"); err != nil || v != "seeded-v1" {
		t.Fatalf("Read(unseeded-key) after its value was put = %q, %v; want seeded-v1", v, err)
	}
}

// TestLocalFromFileFollowsTheFile pins that a file-backed Local is its file: an edit that adds,
// changes or removes a secret is what the next ListSecrets, DescribeSecret and GetSecretValue
// answer, as a write to Secrets Manager is, so the local stack can show a written secret served. A
// file an edit leaves unparseable fails each call, naming the path and never quoting the file, and
// a value put directly stands until the file next changes.
func TestLocalFromFileFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	secret := func(value string) string {
		return `{"secrets": [{"name": "dev/agent-secrets/new-key", "kms_key_id": "alias/dev", "tags": {"owner": "shared", "tier": "agent"}, "value": "` + value + `"}]}`
	}
	write(`{"secrets": []}`)
	local, err := LocalFromFile(path)
	if err != nil {
		t.Fatalf("LocalFromFile: %v", err)
	}
	ctx := context.Background()
	describe := func() error {
		_, err := local.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String("dev/agent-secrets/new-key")})
		return err
	}

	write(secret("new-v1"))
	listed, err := local.ListSecrets(ctx, &secretsmanager.ListSecretsInput{})
	if err != nil || len(listed.SecretList) != 1 || aws.ToString(listed.SecretList[0].Name) != "dev/agent-secrets/new-key" {
		t.Fatalf("ListSecrets after the file gained new-key = %+v, %v; want new-key", listed, err)
	}
	if err := describe(); err != nil {
		t.Fatalf("DescribeSecret(new-key) after the file gained it = %v", err)
	}
	if v, err := (AWS{Client: local}).Read(ctx, "dev/agent-secrets/new-key"); err != nil || v != "new-v1" {
		t.Fatalf("Read(new-key) = %q, %v; want new-v1", v, err)
	}

	write(secret("new-v2"))
	if v, err := (AWS{Client: local}).Read(ctx, "dev/agent-secrets/new-key"); err != nil || v != "new-v2" {
		t.Fatalf("Read(new-key) after its value was edited = %q, %v; want new-v2", v, err)
	}
	local.Put(LocalSecret{Name: "dev/agent-secrets/new-key", KmsKeyID: "alias/dev", Value: "put-v3"})
	if v, err := (AWS{Client: local}).Read(ctx, "dev/agent-secrets/new-key"); err != nil || v != "put-v3" {
		t.Fatalf("Read(new-key) after a Put with the file unchanged = %q, %v; want put-v3", v, err)
	}

	write(`{"secrets": []}`)
	var notFound *types.ResourceNotFoundException
	if err := describe(); !errors.As(err, &notFound) {
		t.Fatalf("DescribeSecret(new-key) after the file dropped it = %v, want ResourceNotFoundException", err)
	}

	const sensitive = "sk-should-never-appear-in-any-error-message"
	write(`{"secrets": [{"name": "dev/agent-secrets/new-key", "value": "` + sensitive)
	if _, err := local.ListSecrets(ctx, &secretsmanager.ListSecretsInput{}); err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), sensitive) {
		t.Fatalf("ListSecrets over a file cut short = %v; want an error naming the path and never quoting the file", err)
	}
}

// currentStages counts the versions entry's SecretVersionsToStages labels AWSCURRENT.
func currentStages(entry types.SecretListEntry) int {
	n := 0
	for _, stages := range entry.SecretVersionsToStages {
		if slices.Contains(stages, "AWSCURRENT") {
			n++
		}
	}
	return n
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
