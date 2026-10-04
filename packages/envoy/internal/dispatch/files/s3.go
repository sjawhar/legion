package files

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
)

// healthTimeout bounds the bucket probe /healthz runs, as the database probe is bounded: a probe
// that answers late is as bad as one that never answers.
const healthTimeout = 2 * time.Second

// s3API is the slice of the S3 client the store uses, so a test can stand the client in.
type s3API interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// S3 is a Store over one bucket.
type S3 struct {
	client s3API
	bucket string
}

// NewS3 stores files in bucket with the AWS SDK's default credential chain. Loading the
// configuration reads only the environment and the shared config files; credentials are fetched
// on the first request.
func NewS3(ctx context.Context, bucket string) (*S3, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithLogger(logging.LoggerFunc(logSDK)))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration for the file store: %w", err)
	}
	return &S3{client: s3.NewFromConfig(cfg), bucket: bucket}, nil
}

// NewS3WithClient is NewS3 over a client the caller built: a test's stand-in, or a client pointed
// at an S3-compatible server.
func NewS3WithClient(client *s3.Client, bucket string) *S3 {
	return &S3{client: client, bucket: bucket}
}

// logSDK hands the AWS SDK's log lines to slog, so they are structured like Dispatch's own and its
// debug lines stay below slog's default level.
func logSDK(classification logging.Classification, format string, v ...any) {
	level := slog.LevelDebug
	if classification == logging.Warn {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "dispatch: aws sdk", "message", fmt.Sprintf(format, v...))
}

// Put writes body under its hash unless the bucket already holds it. The write carries the hash
// as the object's SHA-256 checksum, so S3 refuses a body that does not match it rather than
// storing it under a key that lies about its content.
func (s *S3) Put(ctx context.Context, sha, mime string, body []byte) error {
	if len(body) > MaxObjectSize {
		return fmt.Errorf("put file %s: %d bytes, limit %d", sha, len(body), MaxObjectSize)
	}
	key := Key(sha)
	switch _, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); {
	case err == nil:
		return nil
	case !isNotFound(err):
		return fmt.Errorf("head file %s: %w", sha, err)
	}
	raw, err := hex.DecodeString(sha)
	if err != nil {
		return fmt.Errorf("put file %s: not a hex SHA-256: %w", sha, err)
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		Body:              bytes.NewReader(body),
		ContentLength:     aws.Int64(int64(len(body))),
		ContentType:       aws.String(mime),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		ChecksumSHA256:    aws.String(base64.StdEncoding.EncodeToString(raw)),
	})
	if err != nil {
		return fmt.Errorf("put file %s: %w", sha, err)
	}
	return nil
}

// Get reads the whole object under sha into one buffer of its declared size, at most
// MaxObjectSize, so a request holds the file's bytes and no more.
func (s *S3) Get(ctx context.Context, sha string) ([]byte, error) {
	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(Key(sha))})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get file %s: %w", sha, err)
	}
	if object.Body == nil {
		return nil, fmt.Errorf("get file %s: no body", sha)
	}
	defer object.Body.Close()
	if object.ContentLength == nil {
		return nil, fmt.Errorf("get file %s: no content length", sha)
	}
	size := *object.ContentLength
	if size < 0 || size > MaxObjectSize {
		return nil, fmt.Errorf("get file %s: %d bytes, limit %d", sha, size, MaxObjectSize)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(object.Body, data); err != nil {
		return nil, fmt.Errorf("read file %s, %d bytes declared: %w", sha, size, err)
	}
	var extra [1]byte
	switch _, err := io.ReadFull(object.Body, extra[:]); {
	case err == nil:
		return nil, fmt.Errorf("read file %s: body is longer than the %d bytes declared", sha, size)
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("read file %s: %w", sha, err)
	}
	return data, nil
}

// Healthy asks the bucket for its own metadata, within healthTimeout: a wrong name, a wrong
// region or a missing grant each fail here rather than on the next upload alone.
func (s *S3) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("head bucket %s: %w", s.bucket, err)
	}
	return nil
}

// isNotFound reports whether err is S3 saying the key is absent. HeadObject answers a missing key
// with a bare 404 (NotFound), GetObject with NoSuchKey; a caller without s3:ListBucket gets
// AccessDenied for both, which is a store failure, not an absence.
func isNotFound(err error) bool {
	var missing *types.NoSuchKey
	if errors.As(err, &missing) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NotFound"
}
