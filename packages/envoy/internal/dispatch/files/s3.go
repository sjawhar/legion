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
	"mime"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
)

// S3 is a Store over one bucket.
type S3 struct {
	client *s3.Client
	bucket string
}

var _ Store = (*S3)(nil)

// NewS3 stores files in bucket with the AWS SDK's default credential chain. Loading the
// configuration reads only the environment and the shared config files; credentials are fetched
// on the first request. A configuration that names no region is refused here: the SDK accepts it
// and then fails every call, which would show only on the first upload. The bucket is addressed
// in the request path (`<endpoint>/<bucket>/<key>`), which AWS serves and every S3-compatible
// server takes; the SDK's default, the bucket as a host label, needs a wildcard DNS name in
// front of an endpoint named by AWS_ENDPOINT_URL_S3, which a test container or a loopback
// stand-in has none of.
func NewS3(ctx context.Context, bucket string) (*S3, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithLogger(logging.LoggerFunc(LogSDK)))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration for the file store: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("the file store needs an AWS region: set AWS_REGION, or a region in the shared AWS config")
	}
	return &S3{client: s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true }), bucket: bucket}, nil
}

// LogSDK hands the AWS SDK's log lines to slog, so they are structured like Dispatch's own and its
// debug lines stay below slog's default level. Every AWS client Dispatch builds (this store, the
// retained-asset store in routes) loads its configuration with it, so the SDK logs one way.
func LogSDK(classification logging.Classification, format string, v ...any) {
	level := slog.LevelDebug
	if classification == logging.Warn {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "dispatch: aws sdk", "message", fmt.Sprintf(format, v...))
}

// Put writes body under its hash unless the bucket already holds it, within WriteTimeout. The
// write carries the hash as the object's SHA-256 checksum, so S3 refuses a body that does not
// match it rather than storing it under a key that lies about its content. The object's content
// type is the media type alone (the row keeps the type the client sent, parameters and all, and
// the version route serves the row's), since S3 is stricter than Postgres about what a header
// may hold.
func (s *S3) Put(ctx context.Context, sha, mime string, body []byte) error {
	if len(body) > MaxObjectSize {
		return fmt.Errorf("put file %s: %d bytes, limit %d", sha, len(body), MaxObjectSize)
	}
	// The key is the body's hash; a body that does not hash to it is refused here, before any
	// call, since not every S3-compatible server checks the checksum header S3 does.
	if got := SHA256(body); got != sha {
		return fmt.Errorf("put file %s: the body hashes to %s", sha, got)
	}
	ctx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
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
		ContentType:       aws.String(mediaType(mime)),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		ChecksumSHA256:    aws.String(base64.StdEncoding.EncodeToString(raw)),
	})
	if err != nil {
		return fmt.Errorf("put file %s: %w", sha, err)
	}
	return nil
}

// mediaType is the media type of a Content-Type value, or application/octet-stream when the
// value does not parse as one.
func mediaType(contentType string) string {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil || parsed == "" {
		return "application/octet-stream"
	}
	return parsed
}

// Get opens the object under sha. The call that fetches its headers is bounded by OpenTimeout;
// the body is read at the caller's pace and bounded by ctx alone, and closing it ends the
// request. The body is verified as it is read (VerifyingReader), on top of the SDK's own check of
// the stored checksum, so a body that reaches its end hashed to sha.
func (s *S3) Get(ctx context.Context, sha string) (*Object, error) {
	// The timer bounds the open alone: it is stopped once the headers are in, so the body's
	// transfer is not cut at OpenTimeout, and the context is cancelled when the body is closed.
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(OpenTimeout, cancel)
	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(Key(sha)),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	timer.Stop()
	if err != nil {
		cancel()
		if isNotFound(err) {
			return nil, fmt.Errorf("get file %s: %w", sha, ErrNotFound)
		}
		return nil, fmt.Errorf("get file %s: %w", sha, err)
	}
	if object.Body == nil {
		cancel()
		return nil, fmt.Errorf("get file %s: no body", sha)
	}
	if object.ContentLength == nil {
		_ = object.Body.Close()
		cancel()
		return nil, fmt.Errorf("get file %s: no content length", sha)
	}
	size := *object.ContentLength
	if size < 0 || size > MaxObjectSize {
		_ = object.Body.Close()
		cancel()
		return nil, fmt.Errorf("get file %s: %d bytes, limit %d", sha, size, MaxObjectSize)
	}
	return &Object{
		Body: NewVerifyingReader(&cancelOnClose{ReadCloser: object.Body, cancel: cancel}, sha, size),
		Size: size,
	}, nil
}

// cancelOnClose cancels a body's request context when the body is closed, so a caller that stops
// reading early releases the connection.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
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
