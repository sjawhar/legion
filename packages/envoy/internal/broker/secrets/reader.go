package secrets

import (
	"context"
	"errors"
	"fmt"

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
