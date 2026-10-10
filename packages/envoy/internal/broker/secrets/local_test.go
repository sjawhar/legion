package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// secretsManager is the part of Secrets Manager's client the broker and the agent-secrets CLI call.
// Local answers each call with the client's own signature, so a test can hand either one a Local.
type secretsManager interface {
	ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error)
	DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	CreateSecret(context.Context, *secretsmanager.CreateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	PutSecretValue(context.Context, *secretsmanager.PutSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	TagResource(context.Context, *secretsmanager.TagResourceInput, ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error)
	DeleteSecret(context.Context, *secretsmanager.DeleteSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error)
	RestoreSecret(context.Context, *secretsmanager.RestoreSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error)
}

var (
	_ secretsManager = (*secretsmanager.Client)(nil)
	_ secretsManager = (*Local)(nil)
)

// TestNewLocalClockIsUTC: AWS SDK date fields are UTC, so the fake's deletion clock must not take
// the developer machine's local daylight-saving rules into a recovery deadline.
func TestNewLocalClockIsUTC(t *testing.T) {
	if got := NewLocal().now().Location(); got != time.UTC {
		t.Fatalf("Local.now location = %s, want UTC", got)
	}
}

// TestLocalWriteLifecycle pins Local as a fake Secrets Manager the agent-secrets CLI writes to. A
// create holds the secret with its key, tags and value, and a second create of its name is refused.
// A put replaces its value, by name or by ARN. Tags merge. A delete schedules it for deletion at the
// end of the recovery window (30 days unless named): it is then listed only when planned deletions
// are asked for, DescribeSecret answers its DeletedDate, and its value and every change but a
// restore are refused, as Secrets Manager refuses a secret marked for deletion. A restore serves it
// again.
func TestLocalWriteLifecycle(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	l := NewLocal()
	l.now = func() time.Time { return at }
	name := "example/agent-secrets/new-key"
	id := aws.String(name)
	read := func() (string, error) {
		out, err := l.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: id})
		if err != nil {
			return "", err
		}
		return aws.ToString(out.SecretString), nil
	}
	listed := func(planned bool) []types.SecretListEntry {
		out, err := l.ListSecrets(ctx, &secretsmanager.ListSecretsInput{IncludePlannedDeletion: aws.Bool(planned)})
		if err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
		return out.SecretList
	}
	describe := func() *secretsmanager.DescribeSecretOutput {
		out, err := l.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: id})
		if err != nil {
			t.Fatalf("DescribeSecret: %v", err)
		}
		return out
	}

	created, err := l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: id, KmsKeyId: aws.String("alias/dev"), SecretString: aws.String("v1"),
		Tags: []types.Tag{tag("owner", "ada@example.com"), tag("tier", "agent")},
	})
	if err != nil || aws.ToString(created.ARN) != LocalARN(name) {
		t.Fatalf("CreateSecret = %+v, %v; want it created at %s", created, err, LocalARN(name))
	}
	var exists *types.ResourceExistsException
	if _, err := l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: id, SecretString: aws.String("v2")}); !errors.As(err, &exists) {
		t.Fatalf("a second CreateSecret of %s = %v; want ResourceExistsException", name, err)
	}
	if v, err := read(); err != nil || v != "v1" {
		t.Fatalf("value after the refused create = %q, %v; want v1", v, err)
	}
	if _, err := l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: created.ARN, SecretString: aws.String("v2")}); err != nil {
		t.Fatalf("PutSecretValue by ARN: %v", err)
	}
	if v, err := read(); err != nil || v != "v2" {
		t.Fatalf("value after put = %q, %v; want v2", v, err)
	}
	if _, err := l.TagResource(ctx, &secretsmanager.TagResourceInput{SecretId: id, Tags: []types.Tag{tag("tier", "human")}}); err != nil {
		t.Fatalf("TagResource: %v", err)
	}
	d := describe()
	if tags := tagMap(d.Tags); len(tags) != 2 || tags["owner"] != "ada@example.com" || tags["tier"] != "human" || aws.ToString(d.KmsKeyId) != "alias/dev" {
		t.Fatalf("after retag: key %q, tags %v; want alias/dev, owner kept and tier human", aws.ToString(d.KmsKeyId), tags)
	}

	deleted, err := l.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: id})
	if err != nil || !aws.ToTime(deleted.DeletionDate).Equal(at.Add(30*24*time.Hour)) {
		t.Fatalf("DeleteSecret = %+v, %v; want DeletionDate 30 days after %s", deleted, err, at)
	}
	if got := listed(false); len(got) != 0 {
		t.Fatalf("ListSecrets after delete = %+v; want nothing", got)
	}
	if got := listed(true); len(got) != 1 || !aws.ToTime(got[0].DeletedDate).Equal(at) {
		t.Fatalf("ListSecrets(IncludePlannedDeletion) after delete = %+v; want it with DeletedDate %s", got, at)
	}
	if got := describe().DeletedDate; !aws.ToTime(got).Equal(at) {
		t.Fatalf("DescribeSecret's DeletedDate after delete = %v; want %s", got, at)
	}
	var marked *types.InvalidRequestException
	if v, err := read(); !errors.As(err, &marked) || !strings.Contains(err.Error(), "marked for deletion") || v != "" {
		t.Fatalf("GetSecretValue after delete = %q, %v; want InvalidRequestException naming that it is marked for deletion", v, err)
	}
	for call, err := range map[string]error{
		"PutSecretValue": second(l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: id, SecretString: aws.String("v3")})),
		"TagResource":    second(l.TagResource(ctx, &secretsmanager.TagResourceInput{SecretId: id, Tags: []types.Tag{tag("tier", "agent")}})),
		"DeleteSecret":   second(l.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: id})),
		"CreateSecret":   second(l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: id, SecretString: aws.String("v3")})),
	} {
		if !errors.As(err, &marked) {
			t.Errorf("%s of a secret scheduled for deletion = %v; want InvalidRequestException", call, err)
		}
	}

	if _, err := l.RestoreSecret(ctx, &secretsmanager.RestoreSecretInput{SecretId: id}); err != nil {
		t.Fatalf("RestoreSecret: %v", err)
	}
	if v, err := read(); err != nil || v != "v2" {
		t.Fatalf("value after restore = %q, %v; want v2", v, err)
	}
	if got := describe().DeletedDate; got != nil {
		t.Fatalf("DescribeSecret's DeletedDate after restore = %v; want none", got)
	}
	if got := listed(false); len(got) != 1 || got[0].DeletedDate != nil {
		t.Fatalf("ListSecrets after restore = %+v; want it listed, not scheduled for deletion", got)
	}
	if deleted, err := l.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: id, RecoveryWindowInDays: aws.Int64(7)}); err != nil || !aws.ToTime(deleted.DeletionDate).Equal(at.Add(7*24*time.Hour)) {
		t.Fatalf("DeleteSecret(RecoveryWindowInDays 7) = %+v, %v; want DeletionDate 7 days after %s", deleted, err, at)
	}
}

// TestLocalWritesRefuseWhatSecretsManagerRefuses pins the refusals a write meets: a change to a
// secret Local does not hold is ResourceNotFoundException, and a recovery window outside 7 to 30
// days InvalidParameterException, as in Secrets Manager. Every other request Local cannot answer as
// Secrets Manager would (one naming no secret or no value, an empty or binary value, a client
// request token, a version stage other than AWSCURRENT, a forced deletion) is refused with an error
// naming secrets.Local rather than answered or stored as something else. A secret created with no
// value is held with no version, as Secrets Manager holds one.
func TestLocalWritesRefuseWhatSecretsManagerRefuses(t *testing.T) {
	ctx := context.Background()
	held := aws.String("example/agent-secrets/held")
	l := NewLocal(LocalSecret{Name: *held, Value: "v1"})
	absent := aws.String("example/agent-secrets/absent")
	var notFound *types.ResourceNotFoundException
	for call, err := range map[string]error{
		"PutSecretValue": second(l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: absent, SecretString: aws.String("v")})),
		"TagResource":    second(l.TagResource(ctx, &secretsmanager.TagResourceInput{SecretId: absent, Tags: []types.Tag{tag("tier", "agent")}})),
		"DeleteSecret":   second(l.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: absent})),
		"RestoreSecret":  second(l.RestoreSecret(ctx, &secretsmanager.RestoreSecretInput{SecretId: absent})),
	} {
		if !errors.As(err, &notFound) {
			t.Errorf("%s of a secret Local does not hold = %v; want ResourceNotFoundException", call, err)
		}
	}
	var invalid *types.InvalidParameterException
	for call, err := range map[string]error{
		"DeleteSecret(RecoveryWindowInDays 6)":  second(l.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: held, RecoveryWindowInDays: aws.Int64(6)})),
		"DeleteSecret(RecoveryWindowInDays 31)": second(l.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: held, RecoveryWindowInDays: aws.Int64(31)})),
	} {
		if !errors.As(err, &invalid) {
			t.Errorf("%s = %v; want InvalidParameterException", call, err)
		}
	}
	fresh := aws.String("example/agent-secrets/fresh")
	for call, err := range map[string]error{
		"CreateSecret(no Name)":                    second(l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{SecretString: aws.String("v")})),
		"CreateSecret(empty SecretString)":         second(l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: fresh, SecretString: aws.String("")})),
		"CreateSecret(SecretBinary)":               second(l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: fresh, SecretBinary: []byte("v")})),
		"CreateSecret(ClientRequestToken)":         second(l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: fresh, SecretString: aws.String("v"), ClientRequestToken: aws.String("00000000-0000-4000-8000-000000000001")})),
		"PutSecretValue(no value)":                 second(l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: held})),
		"PutSecretValue(empty SecretString)":       second(l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: held, SecretString: aws.String("")})),
		"PutSecretValue(SecretBinary)":             second(l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: held, SecretBinary: []byte("v")})),
		"PutSecretValue(ClientRequestToken)":       second(l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: held, SecretString: aws.String("v2"), ClientRequestToken: aws.String("00000000-0000-4000-8000-000000000002")})),
		"PutSecretValue(VersionStages AWSPENDING)": second(l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: held, SecretString: aws.String("v2"), VersionStages: []string{"AWSPENDING"}})),
		"DeleteSecret(ForceDeleteWithoutRecovery)": second(l.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: held, ForceDeleteWithoutRecovery: aws.Bool(true)})),
	} {
		if err == nil || !strings.Contains(err.Error(), "secrets.Local") {
			t.Errorf("%s = %v; want a refusal naming secrets.Local", call, err)
		}
	}
	out, err := l.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: held})
	if err != nil || aws.ToString(out.SecretString) != "v1" {
		t.Fatalf("held secret after the refused writes = %v; want it served unchanged", err)
	}
	if _, err := l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: fresh}); err != nil {
		t.Fatalf("CreateSecret with no value after the refused creates = %v; want the name still free", err)
	}
	if _, err := l.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: fresh}); !errors.As(err, &notFound) {
		t.Fatalf("GetSecretValue of a secret created with no value = %v; want ResourceNotFoundException", err)
	}
}

// TestLocalWritesFollowTheFile pins that a write to a LocalFromFile Local lands on the file as it
// stands, as a read does, so an edit made before the write is kept with it, and that a write to a
// Local whose file no longer parses fails naming the path.
func TestLocalWritesFollowTheFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secrets.json")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	write(`{"secrets": [{"name": "dev/agent-secrets/first", "value": "v1"}]}`)
	l, err := LocalFromFile(path)
	if err != nil {
		t.Fatalf("LocalFromFile: %v", err)
	}
	write(`{"secrets": [{"name": "dev/agent-secrets/first", "value": "v1"}, {"name": "dev/agent-secrets/edited", "value": "v1"}]}`)
	if _, err := l.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: aws.String("dev/agent-secrets/created"), SecretString: aws.String("v1")}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	listed, err := l.ListSecrets(ctx, &secretsmanager.ListSecretsInput{})
	if err != nil || len(listed.SecretList) != 3 {
		t.Fatalf("ListSecrets after an edit and a create = %+v, %v; want first, edited and created", listed, err)
	}
	write(`{"secrets": [`)
	if _, err := l.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{SecretId: aws.String("dev/agent-secrets/first"), SecretString: aws.String("v2")}); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("PutSecretValue while the file does not parse = %v; want an error naming %s", err, path)
	}
}

// second is the error of a call answering (output, error).
func second[T any](_ T, err error) error { return err }

func tag(key, value string) types.Tag {
	return types.Tag{Key: aws.String(key), Value: aws.String(value)}
}

func tagMap(tags []types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}
