package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// Local stands in, in memory, for the AWS calls the broker and the agent-secrets CLI make: Secrets
// Manager's ListSecrets, DescribeSecret and GetSecretValue, which the broker reads with, its
// CreateSecret, PutSecretValue, TagResource, DeleteSecret and RestoreSecret, which the CLI writes
// with, and KMS's ListAliases. BROKER_FAKE_SECRETS_FILE loads one for local development
// (LocalFromFile), and tests build one with NewLocal.
type Local struct {
	mu      sync.Mutex
	secrets map[string]LocalSecret
	aliases map[string][]string
	// path is the file a LocalFromFile Local follows, and file the bytes its secrets were last
	// read from; empty for a NewLocal one.
	path string
	file []byte
	// now is the clock DeleteSecret dates a deletion by.
	now func() time.Time
}

// LocalSecret is one secret a Local holds.
type LocalSecret struct {
	// Name is the secret's whole Secrets Manager name, its namespace prefix included.
	Name string `json:"name"`
	// KmsKeyID is the key it is encrypted with, as Secrets Manager reports it; empty is the
	// AWS-managed key.
	KmsKeyID string `json:"kms_key_id"`
	// Tags are its tags.
	Tags map[string]string `json:"tags"`
	// Value is its current value. Empty is a secret created without a value: as in Secrets
	// Manager, it is listed with no version and reading it answers ResourceNotFoundException; one
	// holding a value is listed with a version labelled AWSCURRENT.
	Value string `json:"value"`
	// DeletedAt, when set, is when the secret was deleted with a recovery window: as in Secrets
	// Manager, it is then scheduled for deletion, ListSecrets lists it only when asked for planned
	// deletions, DescribeSecret answers DeletedAt as its DeletedDate, and every other call but
	// RestoreSecret is refused.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// localFile is BROKER_FAKE_SECRETS_FILE's shape.
type localFile struct {
	Secrets []LocalSecret `json:"secrets"`
}

// LocalARN is the ARN a Local gives the secret named name.
func LocalARN(name string) string {
	return "arn:aws:secretsmanager:local:000000000000:secret:" + name
}

// NewLocal holds secrets.
func NewLocal(secrets ...LocalSecret) *Local {
	l := &Local{secrets: map[string]LocalSecret{}, aliases: map[string][]string{}, now: time.Now}
	for _, s := range secrets {
		l.secrets[s.Name] = s
	}
	return l
}

// LocalFromFile reads path, a local-development-only JSON file {"secrets": [LocalSecret, ...]},
// into a Local that follows it: each call reads the file again and, when its bytes have changed,
// answers from it, so an edit to the file is a write as Secrets Manager sees one. A secret Put
// directly, or written with one of Local's write calls, stands until the file next changes. An
// error names path and, for a file that does not parse, only the byte offset and field it failed
// at, never the file's content: no secret value reaches an error message, local development
// included.
func LocalFromFile(path string) (*Local, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	secrets, err := parseLocalFile(path, data)
	if err != nil {
		return nil, err
	}
	l := NewLocal(secrets...)
	l.path, l.file = path, data
	return l, nil
}

// parseLocalFile reads data, the content of path, as BROKER_FAKE_SECRETS_FILE's shape.
func parseLocalFile(path string, data []byte) ([]LocalSecret, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f localFile
	if err := dec.Decode(&f); err != nil {
		var syntax *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return nil, fmt.Errorf("%s: not JSON (at byte %d)", path, syntax.Offset)
		case errors.As(err, &typeErr):
			return nil, fmt.Errorf("%s: %s is not a %s (at byte %d)", path, typeErr.Field, typeErr.Type, typeErr.Offset)
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			return nil, fmt.Errorf("%s: %s", path, strings.TrimPrefix(err.Error(), "json: "))
		default:
			return nil, fmt.Errorf("%s: not a fake secrets file", path)
		}
	}
	for i, s := range f.Secrets {
		if s.Name == "" {
			return nil, fmt.Errorf("%s: secrets[%d] has no name", path, i)
		}
	}
	return f.Secrets, nil
}

// follow reads a LocalFromFile Local's file again and, when its bytes have changed since they were
// last read, takes its secrets from it. A file it cannot read or parse fails the call. The caller
// holds l.mu.
func (l *Local) follow() error {
	if l.path == "" {
		return nil
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", l.path, err)
	}
	if bytes.Equal(data, l.file) {
		return nil
	}
	secrets, err := parseLocalFile(l.path, data)
	if err != nil {
		return err
	}
	l.secrets = make(map[string]LocalSecret, len(secrets))
	for _, s := range secrets {
		l.secrets[s.Name] = s
	}
	l.file = data
	return nil
}

// Put adds secret, or replaces the one of its name.
func (l *Local) Put(secret LocalSecret) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secrets[secret.Name] = secret
}

// Delete removes the secret named name.
func (l *Local) Delete(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.secrets, name)
}

// Alias points the KMS alias alias ("alias/<name>") at the key whose ARN is keyARN.
func (l *Local) Alias(keyARN, alias string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.aliases[keyARN] = append(l.aliases[keyARN], alias)
}

// ListSecrets answers every secret in one page, filtered as Secrets Manager filters by name: each
// name filter value matches a name it prefixes, case-sensitively. A secret holding a value is
// listed with one version labelled AWSCURRENT, and one with no value with none. A secret scheduled
// for deletion is left out unless IncludePlannedDeletion asks for it, and then listed with its
// DeletedDate, as in Secrets Manager.
func (l *Local) ListSecrets(_ context.Context, in *secretsmanager.ListSecretsInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.follow(); err != nil {
		return nil, err
	}
	out := &secretsmanager.ListSecretsOutput{}
	for _, s := range l.secrets {
		if (s.DeletedAt != nil && !aws.ToBool(in.IncludePlannedDeletion)) || !matchesNameFilters(s.Name, in.Filters) {
			continue
		}
		out.SecretList = append(out.SecretList, types.SecretListEntry{
			Name: aws.String(s.Name), ARN: aws.String(LocalARN(s.Name)), KmsKeyId: s.kmsKeyID(),
			Tags: s.awsTags(), SecretVersionsToStages: s.versionsToStages(), DeletedDate: s.DeletedAt,
		})
	}
	return out, nil
}

// DescribeSecret answers the secret SecretId names, by name or by LocalARN, with the tags, key and
// version stages ListSecrets lists it with, and DeletedDate while it is scheduled for deletion;
// ResourceNotFoundException for one it does not hold.
func (l *Local) DescribeSecret(_ context.Context, in *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.held(in.SecretId)
	if err != nil {
		return nil, err
	}
	return &secretsmanager.DescribeSecretOutput{
		Name: aws.String(s.Name), ARN: aws.String(LocalARN(s.Name)), KmsKeyId: s.kmsKeyID(),
		Tags: s.awsTags(), VersionIdsToStages: s.versionsToStages(), DeletedDate: s.DeletedAt,
	}, nil
}

// held follows the file and answers the secret id names, by name or by LocalARN;
// ResourceNotFoundException for one Local does not hold. The caller holds l.mu.
func (l *Local) held(id *string) (LocalSecret, error) {
	if err := l.follow(); err != nil {
		return LocalSecret{}, err
	}
	s, ok := l.secrets[strings.TrimPrefix(aws.ToString(id), LocalARN(""))]
	if !ok {
		return LocalSecret{}, &types.ResourceNotFoundException{Message: aws.String("Secrets Manager can't find the specified secret.")}
	}
	return s, nil
}

// changeable is held, and InvalidRequestException for a secret scheduled for deletion, which
// Secrets Manager refuses to read or change until it is restored. The caller holds l.mu.
func (l *Local) changeable(id *string) (LocalSecret, error) {
	s, err := l.held(id)
	if err != nil {
		return LocalSecret{}, err
	}
	if s.DeletedAt != nil {
		return LocalSecret{}, &types.InvalidRequestException{Message: aws.String("You can't perform this operation on the secret because it was marked for deletion.")}
	}
	return s, nil
}

// kmsKeyID is s's KmsKeyId as Secrets Manager reports it: absent for the AWS-managed key.
func (s LocalSecret) kmsKeyID() *string {
	if s.KmsKeyID == "" {
		return nil
	}
	return aws.String(s.KmsKeyID)
}

func (s LocalSecret) awsTags() []types.Tag {
	tags := make([]types.Tag, 0, len(s.Tags))
	for k, v := range s.Tags {
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return tags
}

// localVersion is the version id of the one version a secret holding a value has.
const localVersion = "local-current"

// versionsToStages is one version labelled AWSCURRENT for a secret holding a value, none otherwise.
func (s LocalSecret) versionsToStages() map[string][]string {
	if s.Value == "" {
		return nil
	}
	return map[string][]string{localVersion: {"AWSCURRENT"}}
}

func matchesNameFilters(name string, filters []types.Filter) bool {
	for _, f := range filters {
		if f.Key != types.FilterNameStringTypeName {
			continue
		}
		matched := false
		for _, v := range f.Values {
			matched = matched || strings.HasPrefix(name, v)
		}
		if !matched {
			return false
		}
	}
	return true
}

// GetSecretValue answers the AWSCURRENT value of the secret SecretId names, by name or by
// LocalARN; ResourceNotFoundException for one it does not hold or that has no value, and
// InvalidRequestException for one scheduled for deletion, as Secrets Manager refuses that one.
func (l *Local) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.changeable(in.SecretId)
	if err != nil {
		return nil, err
	}
	if s.Value == "" {
		return nil, &types.ResourceNotFoundException{Message: aws.String("Secrets Manager can't find the specified secret value for staging label: AWSCURRENT")}
	}
	return &secretsmanager.GetSecretValueOutput{Name: aws.String(s.Name), ARN: aws.String(LocalARN(s.Name)), SecretString: aws.String(s.Value)}, nil
}

// CreateSecret holds a new secret named Name with KmsKeyId, Tags and SecretString as its value,
// answering its ARN and, when it was given a value, the version holding it; one created with no
// SecretString has no version, as in Secrets Manager. A name Local holds is
// ResourceExistsException, or InvalidRequestException while its secret is scheduled for deletion,
// as Secrets Manager answers both. Local holds a name, a key, tags and a non-empty string value:
// a request naming no secret, an empty or binary value, a description, replica regions or a type
// is refused.
func (l *Local) CreateSecret(_ context.Context, in *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	name := aws.ToString(in.Name)
	switch {
	case name == "":
		return nil, refuse("CreateSecret names no secret")
	case in.SecretString != nil && *in.SecretString == "":
		return nil, refuse("an empty SecretString is no value Local can hold; leave it out to create a secret with no value")
	case in.SecretBinary != nil || in.Description != nil || len(in.AddReplicaRegions) > 0 || in.Type != nil:
		return nil, refuse("CreateSecret holds a name, a key, tags and a string value only")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.follow(); err != nil {
		return nil, err
	}
	if s, ok := l.secrets[name]; ok {
		if s.DeletedAt != nil {
			return nil, &types.InvalidRequestException{Message: aws.String("You can't create this secret because a secret with this name is already scheduled for deletion.")}
		}
		return nil, &types.ResourceExistsException{Message: aws.String("The operation failed because the secret " + name + " already exists.")}
	}
	s := LocalSecret{Name: name, KmsKeyID: aws.ToString(in.KmsKeyId), Tags: withTags(nil, in.Tags), Value: aws.ToString(in.SecretString)}
	l.secrets[name] = s
	out := &secretsmanager.CreateSecretOutput{Name: aws.String(name), ARN: aws.String(LocalARN(name))}
	if s.Value != "" {
		out.VersionId = aws.String(localVersion)
	}
	return out, nil
}

// PutSecretValue makes SecretString the AWSCURRENT value of the secret SecretId names, by name or
// by LocalARN; ResourceNotFoundException for one Local does not hold and InvalidRequestException
// for one scheduled for deletion, as in Secrets Manager. Local holds one string value, labelled
// AWSCURRENT: an empty or binary value, a rotation token or another version stage is refused.
func (l *Local) PutSecretValue(_ context.Context, in *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	if aws.ToString(in.SecretString) == "" || in.SecretBinary != nil || in.RotationToken != nil {
		return nil, refuse("PutSecretValue takes a non-empty SecretString only")
	}
	for _, stage := range in.VersionStages {
		if stage != "AWSCURRENT" {
			return nil, refuse("a secret's one version is labelled AWSCURRENT, not " + stage)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.changeable(in.SecretId)
	if err != nil {
		return nil, err
	}
	s.Value = *in.SecretString
	l.secrets[s.Name] = s
	return &secretsmanager.PutSecretValueOutput{
		Name: aws.String(s.Name), ARN: aws.String(LocalARN(s.Name)), VersionId: aws.String(localVersion), VersionStages: []string{"AWSCURRENT"},
	}, nil
}

// TagResource sets Tags on the secret SecretId names, keeping the tags it does not name, as Secrets
// Manager appends tags; refused as PutSecretValue is for a secret Local does not hold or one
// scheduled for deletion.
func (l *Local) TagResource(_ context.Context, in *secretsmanager.TagResourceInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.changeable(in.SecretId)
	if err != nil {
		return nil, err
	}
	s.Tags = withTags(s.Tags, in.Tags)
	l.secrets[s.Name] = s
	return &secretsmanager.TagResourceOutput{}, nil
}

// withTags is a copy of held with tags set over it, so no map a caller handed Local changes.
func withTags(held map[string]string, tags []types.Tag) map[string]string {
	merged := make(map[string]string, len(held)+len(tags))
	maps.Copy(merged, held)
	for _, t := range tags {
		merged[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return merged
}

// DeleteSecret schedules the secret SecretId names for deletion: its DeletedAt becomes now, and it
// answers DeletionDate, the end of the recovery window of RecoveryWindowInDays (30 when unset),
// until which RestoreSecret brings it back. A window outside 7 to 30 days is
// InvalidParameterException, and a secret Local does not hold or one already scheduled for
// deletion is refused as PutSecretValue refuses it, as in Secrets Manager. ForceDeleteWithoutRecovery
// is refused: Local schedules every deletion.
func (l *Local) DeleteSecret(_ context.Context, in *secretsmanager.DeleteSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	if aws.ToBool(in.ForceDeleteWithoutRecovery) {
		return nil, refuse("DeleteSecret schedules a deletion with a recovery window; ForceDeleteWithoutRecovery is not modelled")
	}
	days := int64(30)
	if in.RecoveryWindowInDays != nil {
		days = *in.RecoveryWindowInDays
		if days < 7 || days > 30 {
			return nil, &types.InvalidParameterException{Message: aws.String("The RecoveryWindowInDays value must be between 7 and 30 days (inclusive).")}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.changeable(in.SecretId)
	if err != nil {
		return nil, err
	}
	deletedAt := l.now()
	s.DeletedAt = &deletedAt
	l.secrets[s.Name] = s
	return &secretsmanager.DeleteSecretOutput{
		Name: aws.String(s.Name), ARN: aws.String(LocalARN(s.Name)), DeletionDate: aws.Time(deletedAt.Add(time.Duration(days) * 24 * time.Hour)),
	}, nil
}

// RestoreSecret cancels the scheduled deletion of the secret SecretId names, so it is listed and
// read again; ResourceNotFoundException for one Local does not hold. A secret not scheduled for
// deletion is left as it is.
func (l *Local) RestoreSecret(_ context.Context, in *secretsmanager.RestoreSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.held(in.SecretId)
	if err != nil {
		return nil, err
	}
	s.DeletedAt = nil
	l.secrets[s.Name] = s
	return &secretsmanager.RestoreSecretOutput{Name: aws.String(s.Name), ARN: aws.String(LocalARN(s.Name))}, nil
}

// refuse is Local's refusal of a request it cannot answer as Secrets Manager would, named as
// Local's so it never reads as Secrets Manager's own answer.
func refuse(why string) error {
	return errors.New("secrets.Local: " + why)
}

// ListAliases answers, in one page, the aliases Alias pointed at the key KeyId names by its ARN.
func (l *Local) ListAliases(_ context.Context, in *kms.ListAliasesInput, _ ...func(*kms.Options)) (*kms.ListAliasesOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	keyARN := aws.ToString(in.KeyId)
	out := &kms.ListAliasesOutput{}
	for _, alias := range l.aliases[keyARN] {
		aliasARN := keyARN[:strings.LastIndex(keyARN, ":")+1] + alias
		out.Aliases = append(out.Aliases, kmstypes.AliasListEntry{AliasName: aws.String(alias), AliasArn: aws.String(aliasARN)})
	}
	return out, nil
}
