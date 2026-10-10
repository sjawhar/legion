package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
)

const testBucket = "example-files-bucket"

// s3Store serves a fake bucket and opens the store over it with files.NewS3, the constructor a
// deployment runs, so what the store sends is what production sends.
func s3Store(t *testing.T, bucket string) (*files.S3, *filestest.S3) {
	t.Helper()
	fake := filestest.ServeS3(t, testBucket)
	store, err := files.NewS3(context.Background(), bucket)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return store, fake
}

func TestS3StoresAFileOnceByItsHashAndReadsItBack(t *testing.T) {
	store, fake := s3Store(t, testBucket)
	ctx := context.Background()
	body := bytes.Repeat([]byte("dispatch "), 100_000)
	sha := files.SHA256(body)

	// S3 answers the HEAD of a key it does not hold with a bare 404, no NoSuchKey: the first Put
	// reads that as absence and writes.
	if err := store.Put(ctx, sha, "image/png", body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	mime, stored, held := fake.Object(files.Key(sha))
	if !held {
		t.Fatalf("Put stored nothing under %s", files.Key(sha))
	}
	if mime != "image/png" {
		t.Errorf("stored content type = %q, want image/png", mime)
	}
	if !bytes.Equal(stored, body) {
		t.Fatalf("stored %d bytes, want %d identical bytes", len(stored), len(body))
	}

	// A second upload of the same bytes writes nothing.
	before := len(fake.Requests())
	if err := store.Put(ctx, sha, "image/png", body); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	for _, request := range fake.Requests()[before:] {
		if strings.HasPrefix(request, "PUT ") {
			t.Errorf("second Put of held bytes wrote them again: %s", request)
		}
	}

	object, err := store.Get(ctx, sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil || object.Size != int64(len(body)) || !bytes.Equal(got, body) {
		t.Fatalf("Get read %d bytes (size %d, %v), want %d identical bytes", len(got), object.Size, err, len(body))
	}
	if err := store.Healthy(ctx); err != nil {
		t.Errorf("Healthy: %v", err)
	}
}

func TestS3AnswersNotFoundForAHashItDoesNotHold(t *testing.T) {
	store, _ := s3Store(t, testBucket)
	_, err := store.Get(context.Background(), strings.Repeat("ab", 32))
	if !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Get of an absent hash: %v, want ErrNotFound", err)
	}
}

// A caller whose grant lacks s3:ListBucket gets AccessDenied for a missing key, and a caller
// with no grant for any key: a store failure, which the version route answers 502, never an
// absent object, which it answers 500 FILE_MISSING and a backfill would read as a lost file.
func TestS3ReadsAccessDeniedAsAFailureNotAnAbsence(t *testing.T) {
	store, fake := s3Store(t, testBucket)
	fake.Deny()
	ctx := context.Background()
	sha := strings.Repeat("ab", 32)
	_, err := store.Get(ctx, sha)
	if err == nil || errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Get under AccessDenied: %v, want a failure that is not ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("Get under AccessDenied: %v, want the refusal named", err)
	}
	// Put checks with HeadObject first, and a HEAD's refusal has no body for the SDK to read the
	// code from, so it names the status instead.
	if err := store.Put(ctx, files.SHA256([]byte("x")), "text/plain", []byte("x")); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("Put under AccessDenied: %v, want the refusal named, not a write past it", err)
	}
}

// A body that answers (0, nil) without ending would keep the reader's loop, and the response
// copying from it, spinning; after bufio's hundred empty reads it fails with io.ErrNoProgress.
func TestVerifyingReaderFailsABodyThatMakesNoProgress(t *testing.T) {
	reader := files.NewVerifyingReader(io.NopCloser(stuck{}), files.SHA256([]byte("never")), 5)
	n, err := reader.Read(make([]byte, 8))
	if n != 0 || !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("Read of a stuck body = %d, %v; want 0, io.ErrNoProgress", n, err)
	}
	if _, again := reader.Read(make([]byte, 8)); !errors.Is(again, io.ErrNoProgress) {
		t.Fatalf("a second Read after the failure = %v; want the same failure kept", again)
	}
}

// stuck is an io.Reader that reads nothing and never ends.
type stuck struct{}

func (stuck) Read([]byte) (int, error) { return 0, nil }

func TestS3SendsTheHashAsTheObjectsChecksum(t *testing.T) {
	store, fake := s3Store(t, testBucket)
	body := []byte("the bytes")
	wrong := files.SHA256([]byte("other bytes"))
	// The bucket verifies the checksum header as S3 does, so a body stored under another body's
	// hash is refused, not stored under a key that lies about its content.
	if err := store.Put(context.Background(), wrong, "text/plain", body); err == nil {
		t.Fatal("Put stored a body under a hash that is not its own")
	}
	if _, _, held := fake.Object(files.Key(wrong)); held {
		t.Fatal("the refused object was stored")
	}
}

func TestS3RefusesAnObjectPastTheSizeLimit(t *testing.T) {
	store, fake := s3Store(t, testBucket)
	sha := strings.Repeat("cd", 32)
	fake.SetObject(files.Key(sha), "application/octet-stream", make([]byte, files.MaxObjectSize+1))
	if _, err := store.Get(context.Background(), sha); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Get of an oversize object: %v, want a size error", err)
	}
	if err := store.Put(context.Background(), sha, "application/octet-stream", make([]byte, files.MaxObjectSize+1)); err == nil {
		t.Fatal("Put accepted an oversize body")
	}
}

// An object whose bytes are not what its key says (a corrupted or replaced object) reads with an
// error before its end, through the real client, and the reader hands out fewer bytes than the
// object holds before it says so: a caller copying it to a client with a Content-Length leaves
// that client short, which every HTTP client reports, rather than whole with the wrong bytes.
func TestS3RefusesABodyThatDoesNotHashToItsKey(t *testing.T) {
	store, fake := s3Store(t, testBucket)
	same := bytes.Repeat([]byte("x"), 200_000)
	sha := files.SHA256(same)
	tampered := append(bytes.Repeat([]byte("x"), 199_999), 'y') // the same length, other bytes
	fake.SetObject(files.Key(sha), "text/plain", tampered)
	for _, chunk := range []int{1, 4096, 1 << 20} {
		object, err := store.Get(context.Background(), sha)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		var delivered int64
		var readErr error
		buffer := make([]byte, chunk)
		for readErr == nil {
			n, err := object.Body.Read(buffer)
			delivered += int64(n)
			readErr = err
		}
		_ = object.Body.Close()
		if !strings.Contains(readErr.Error(), "reads back as") {
			t.Fatalf("reading a tampered object in %d-byte reads: %v, want a hash mismatch", chunk, readErr)
		}
		if delivered >= int64(len(tampered)) {
			t.Fatalf("reading a tampered object in %d-byte reads handed out %d of %d bytes before refusing; want fewer", chunk, delivered, len(tampered))
		}
	}
}

func TestS3HealthyFailsForABucketThatDoesNotExist(t *testing.T) {
	store, fake := s3Store(t, "no-such-bucket")
	if err := store.Healthy(context.Background()); err == nil {
		t.Fatal("Healthy answered nil for a bucket that does not exist")
	}
	if requests := fake.Requests(); len(requests) == 0 || requests[0] != "HEAD /no-such-bucket" {
		t.Fatalf("Healthy sent %q, want a HEAD of the bucket", requests)
	}
}
