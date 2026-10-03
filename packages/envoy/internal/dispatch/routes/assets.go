package routes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	maxRetainedAssetSize     int64 = 8 << 20
	retainedAssetFetchTimeout      = 3 * time.Second
)

// ErrAssetNotFound distinguishes an absent retained asset from a store failure.
var ErrAssetNotFound = errors.New("retained asset not found")

// RetainedAsset is an immutable asset fetched from a prior dashboard build.
type RetainedAsset struct {
	Body          io.ReadCloser
	ContentLength int64
}

// AssetStore provides retained assets after the current dashboard build misses locally.
type AssetStore interface {
	GetAsset(ctx context.Context, key string) (RetainedAsset, error)
}

type s3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type s3AssetStore struct {
	client s3API
	bucket string
}

// NewS3AssetStore uses the AWS SDK's default credential chain to read retained assets.
func NewS3AssetStore(ctx context.Context, bucket string) (AssetStore, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration for retained assets: %w", err)
	}
	return &s3AssetStore{client: s3.NewFromConfig(cfg), bucket: bucket}, nil
}

func (s *s3AssetStore) GetAsset(ctx context.Context, key string) (RetainedAsset, error) {
	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var missing *types.NoSuchKey
		if errors.As(err, &missing) {
			return RetainedAsset{}, ErrAssetNotFound
		}
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return RetainedAsset{}, ErrAssetNotFound
		}
		return RetainedAsset{}, fmt.Errorf("get retained asset %q: %w", key, err)
	}
	if object.Body == nil {
		return RetainedAsset{}, fmt.Errorf("get retained asset %q: empty response body", key)
	}
	if object.ContentLength == nil || *object.ContentLength < 0 {
		_ = object.Body.Close()
		return RetainedAsset{}, fmt.Errorf("get retained asset %q: missing content length", key)
	}
	return RetainedAsset{Body: object.Body, ContentLength: *object.ContentLength}, nil
}
