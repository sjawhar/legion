package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// Local stands in, in memory, for the AWS calls the broker makes: Secrets Manager's ListSecrets
// and GetSecretValue, and KMS's ListAliases. BROKER_FAKE_SECRETS_FILE loads one for local
// development (LocalFromFile), and tests build one with NewLocal.
type Local struct {
	mu      sync.Mutex
	secrets map[string]LocalSecret
	aliases map[string][]string
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
	// Value is its value.
	Value string `json:"value"`
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
	l := &Local{secrets: map[string]LocalSecret{}, aliases: map[string][]string{}}
	for _, s := range secrets {
		l.secrets[s.Name] = s
	}
	return l
}

// LocalFromFile reads path, a local-development-only JSON file {"secrets": [LocalSecret, ...]},
// into a Local. An error names path and, for a file that does not parse, only the byte offset and
// field it failed at, never the file's content: no secret value reaches an error message, local
// development included.
func LocalFromFile(path string) (*Local, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
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
	return NewLocal(f.Secrets...), nil
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
// name filter value matches a name it prefixes, case-sensitively.
func (l *Local) ListSecrets(_ context.Context, in *secretsmanager.ListSecretsInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := &secretsmanager.ListSecretsOutput{}
	for _, s := range l.secrets {
		if !matchesNameFilters(s.Name, in.Filters) {
			continue
		}
		entry := types.SecretListEntry{Name: aws.String(s.Name), ARN: aws.String(LocalARN(s.Name))}
		if s.KmsKeyID != "" {
			entry.KmsKeyId = aws.String(s.KmsKeyID)
		}
		for k, v := range s.Tags {
			entry.Tags = append(entry.Tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		out.SecretList = append(out.SecretList, entry)
	}
	return out, nil
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

// GetSecretValue answers the value of the secret SecretId names, by name or by LocalARN, and
// ResourceNotFoundException for one it does not hold.
func (l *Local) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := aws.ToString(in.SecretId)
	s, ok := l.secrets[strings.TrimPrefix(id, LocalARN(""))]
	if !ok {
		return nil, &types.ResourceNotFoundException{Message: aws.String("Secrets Manager can't find the specified secret.")}
	}
	return &secretsmanager.GetSecretValueOutput{Name: aws.String(s.Name), ARN: aws.String(LocalARN(s.Name)), SecretString: aws.String(s.Value)}, nil
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
