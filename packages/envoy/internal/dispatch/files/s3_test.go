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
// error before its end, through the real client, so no caller can copy it to a client whole.
func TestS3RefusesABodyThatDoesNotHashToItsKey(t *testing.T) {
	store, fake := s3Store(t, testBucket)
	sha := files.SHA256([]byte("what the key says"))
	fake.SetObject(files.Key(sha), "text/plain", []byte("what is stored"))
	object, err := store.Get(context.Background(), sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer object.Body.Close()
	if _, err := io.ReadAll(object.Body); err == nil || !strings.Contains(err.Error(), "reads back as") {
		t.Fatalf("reading a corrupted object: %v, want a hash mismatch", err)
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
