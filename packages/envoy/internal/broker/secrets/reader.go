package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

var ErrNotFound = errors.New("secret not found in the store")

type Reader interface {
	Read(ctx context.Context, source string) (string, error)
}

type smAPI interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type AWS struct{ Client smAPI }

func (a AWS) Read(ctx context.Context, source string) (string, error) {
	out, err := a.Client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(source)})
	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("secrets manager %s: %w", source, err)
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", fmt.Errorf("secrets manager %s: value is not a non-empty string", source)
	}
	return *out.SecretString, nil
}

type Fake map[string]string

func (f Fake) Read(_ context.Context, source string) (string, error) {
	v, ok := f[source]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// FakeFromFile reads path as a local-development-only .env-style file — one NAME=value line,
// blank lines and #-prefixed comments skipped, no quoting or escaping — into a Fake. A read
// error or a line with no "=" is a loud error naming path (and, for a malformed line, its line
// number), never a silently skipped line: production never calls this (BROKER_RULES_S3_URI
// selects AWS instead), so a bad fixture must fail the boot that reads it, not start broken.
func FakeFromFile(path string) (Fake, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	fake := Fake{}
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: not a NAME=value line: %q", path, i+1, line)
		}
		fake[strings.TrimSpace(name)] = value
	}
	return fake, nil
}
