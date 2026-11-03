package files

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	storage "google.golang.org/api/storage/v1"
)

// GCS is a Store over one Google Cloud Storage bucket, through the JSON API. It needs exactly two
// permissions on the bucket, storage.objects.create and storage.objects.get (roles/storage.objectCreator
// and roles/storage.objectViewer cover them), and never overwrites or deletes an object: every
// write carries a create-only precondition, and no call it makes deletes.
type GCS struct {
	objects *storage.ObjectsService
	bucket  string
}

var _ Store = (*GCS)(nil)

// healthProbeSHA names the key Healthy reads: an object no upload writes, since no body's
// SHA-256 is 64 zeros. Its absence is the answer a healthy bucket gives.
var healthProbeSHA = strings.Repeat("0", 64)

// NewGCS stores files in bucket with Application Default Credentials: on GKE, the pod's Workload
// Identity principal; elsewhere GOOGLE_APPLICATION_CREDENTIALS or a gcloud login, which the client
// library finds for itself. Credentials are found here and a token is fetched on the first
// request, so construction makes no call. opts are passed to the client after Dispatch's own and
// exist for the tests, which point it at a loopback bucket (filestest.ServeGCS); a deployment
// passes none.
func NewGCS(ctx context.Context, bucket string, opts ...option.ClientOption) (*GCS, error) {
	if err := validGCSBucket(bucket); err != nil {
		return nil, err
	}
	service, err := storage.NewService(ctx, append([]option.ClientOption{option.WithScopes(storage.DevstorageReadWriteScope)}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("open the Google Cloud Storage client for the file store: %w", err)
	}
	return &GCS{objects: service.Objects, bucket: bucket}, nil
}

// Put writes body under its hash unless the bucket already holds it, within WriteTimeout. It reads
// the key's metadata first, so the bytes of a file uploaded again are not sent again; then it
// writes in one request with ifGenerationMatch=0, so a concurrent writer that stored the key in
// between makes GCS answer 412, which is the object already held, and nothing is ever
// overwritten. The write carries the body's CRC32C and MD5 in its metadata, which GCS checks
// before it stores the object. The object's content type is the media type alone, as S3's.
func (s *GCS) Put(ctx context.Context, sha, mime string, body []byte) error {
	if len(body) > MaxObjectSize {
		return fmt.Errorf("put file %s: %d bytes, limit %d", sha, len(body), MaxObjectSize)
	}
	if got := SHA256(body); got != sha {
		return fmt.Errorf("put file %s: the body hashes to %s", sha, got)
	}
	ctx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
	key := Key(sha)
	switch _, err := s.objects.Get(s.bucket, key).Fields("name").Context(ctx).Do(); {
	case err == nil:
		return nil
	case !isGCSNotFound(err):
		return fmt.Errorf("read file %s's metadata: %w", sha, err)
	}
	md5Sum := md5.Sum(body)
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)))
	object := &storage.Object{
		Name:        key,
		ContentType: mediaType(mime),
		Crc32c:      base64.StdEncoding.EncodeToString(crc),
		Md5Hash:     base64.StdEncoding.EncodeToString(md5Sum[:]),
	}
	// ChunkSize(0) sends the object and its metadata in one multipart request, the upload whose
	// metadata hashes GCS verifies; a resumable upload would split the body across requests.
	_, err := s.objects.Insert(s.bucket, object).
		Media(bytes.NewReader(body), googleapi.ChunkSize(0)).
		IfGenerationMatch(0).
		Fields("name").
		Context(ctx).
		Do()
	if err == nil || gcsStatus(err) == http.StatusPreconditionFailed {
		return nil
	}
	return fmt.Errorf("put file %s: %w", sha, err)
}

// Get opens the object under sha. The request that fetches its headers is bounded by
// OpenTimeout; the body is read at the caller's pace and bounded by ctx alone, and closing it ends
// the request. The body is verified as it is read (VerifyingReader).
func (s *GCS) Get(ctx context.Context, sha string) (*Object, error) {
	ctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(OpenTimeout, cancel)
	response, err := s.objects.Get(s.bucket, Key(sha)).Context(ctx).Download()
	timer.Stop()
	if err != nil {
		cancel()
		if isGCSNotFound(err) {
			return nil, fmt.Errorf("get file %s: %w", sha, ErrNotFound)
		}
		return nil, fmt.Errorf("get file %s: %w", sha, err)
	}
	size := response.ContentLength
	if size < 0 {
		_ = response.Body.Close()
		cancel()
		return nil, fmt.Errorf("get file %s: no content length", sha)
	}
	if size > MaxObjectSize {
		_ = response.Body.Close()
		cancel()
		return nil, fmt.Errorf("get file %s: %d bytes, limit %d", sha, size, MaxObjectSize)
	}
	return &Object{
		Body: NewVerifyingReader(&cancelOnClose{ReadCloser: response.Body, cancel: cancel}, sha, size),
		Size: size,
	}, nil
}

// Healthy reads the metadata of a key no upload writes, within healthTimeout, which needs only
// storage.objects.get: GCS answering that the object does not exist is a healthy bucket, while a
// bucket that does not exist, a missing grant or a credential that does not work each fail here
// rather than on the next upload alone. Reading the bucket's own metadata would need
// storage.buckets.get, which the store's grant does not carry.
func (s *GCS) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	_, err := s.objects.Get(s.bucket, Key(healthProbeSHA)).Fields("name").Context(ctx).Do()
	if err == nil || isGCSNotFound(err) {
		return nil
	}
	return fmt.Errorf("read an object's metadata in bucket %s: %w", s.bucket, err)
}

// gcsStatus is the HTTP status of a GCS error, or 0 for an error that carries none.
func gcsStatus(err error) int {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return 0
}

// isGCSNotFound reports whether err is GCS saying the object is absent. GCS answers a missing
// object and a missing bucket alike, 404 with the reason notFound, and tells them apart only in
// the message ("No such object: …" against "The specified bucket does not exist."), which a media
// download carries as its plain-text body; a missing bucket is a store failure, never an absent
// file, so it is not read as one. A caller without storage.objects.get gets 403 for both.
func isGCSNotFound(err error) bool {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
		return false
	}
	return !strings.Contains(apiErr.Message+" "+apiErr.Body, "bucket does not exist")
}
