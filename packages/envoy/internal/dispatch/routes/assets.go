package routes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/logging"
)

const (
	// maxRetainedAssetSize bounds what one request may read from the store, and so the memory one
	// retained miss holds while it is answered.
	maxRetainedAssetSize int64 = 8 << 20
	// retainedAssetFetchTimeout bounds the whole read of a retained asset from the store, object
	// body included. The browser's download of the bytes read is not bounded by it.
	retainedAssetFetchTimeout = 3 * time.Second
	// maxRetainedBytesHeld caps the bytes of retained objects the handler holds at once. A client
	// that stops reading keeps its object in memory, and the server sets no write timeout.
	maxRetainedBytesHeld int64 = 64 << 20
)

// ErrAssetNotFound distinguishes an absent retained asset from a store failure.
var ErrAssetNotFound = errors.New("retained asset not found")

// AssetStore holds the hashed assets of earlier dashboard builds, which the static handler serves
// when the current build does not hold the file a browser asks for.
type AssetStore interface {
	// GetAsset returns the whole object stored under key, at most maxRetainedAssetSize bytes, or
	// ErrAssetNotFound when there is none. Any other error is a store failure.
	GetAsset(ctx context.Context, key string) ([]byte, error)
}

type s3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type s3AssetStore struct {
	client s3API
	bucket string
}

// NewS3AssetStore reads retained assets from bucket with the AWS SDK's default credential chain.
// Loading it reads only the environment and shared config files; credentials are fetched on the
// first request.
func NewS3AssetStore(ctx context.Context, bucket string) (AssetStore, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithLogger(logging.LoggerFunc(logSDK)))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration for retained assets: %w", err)
	}
	return &s3AssetStore{client: s3.NewFromConfig(cfg), bucket: bucket}, nil
}

// logSDK hands the AWS SDK's log lines to slog, so they are structured like Dispatch's own and its
// debug lines (an object stored without a checksum) stay below slog's default level.
func logSDK(classification logging.Classification, format string, v ...any) {
	level := slog.LevelDebug
	if classification == logging.Warn {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "dispatch: aws sdk", "message", fmt.Sprintf(format, v...))
}

func (s *s3AssetStore) GetAsset(ctx context.Context, key string) ([]byte, error) {
	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// S3 answers a missing key with NoSuchKey only to a caller that may also list the bucket;
		// without s3:ListBucket it is AccessDenied, a store failure.
		var missing *types.NoSuchKey
		if errors.As(err, &missing) {
			return nil, ErrAssetNotFound
		}
		return nil, fmt.Errorf("get retained asset %q: %w", key, err)
	}
	if object.Body == nil {
		return nil, fmt.Errorf("get retained asset %q: no body", key)
	}
	defer object.Body.Close()
	if object.ContentLength == nil {
		return nil, fmt.Errorf("get retained asset %q: no content length", key)
	}
	size := *object.ContentLength
	if size < 0 || size > maxRetainedAssetSize {
		return nil, fmt.Errorf("get retained asset %q: %d bytes, limit %d", key, size, maxRetainedAssetSize)
	}
	// One buffer of the declared size, so a request holds the object's bytes and no more.
	data := make([]byte, size)
	if _, err := io.ReadFull(object.Body, data); err != nil {
		return nil, fmt.Errorf("read retained asset %q, %d bytes declared: %w", key, size, err)
	}
	var extra [1]byte
	switch _, err := io.ReadFull(object.Body, extra[:]); {
	case err == nil:
		return nil, fmt.Errorf("read retained asset %q: body is longer than the %d bytes declared", key, size)
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("read retained asset %q: %w", key, err)
	}
	return data, nil
}
