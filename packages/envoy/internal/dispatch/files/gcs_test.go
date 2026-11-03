package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
)

// gcsStore serves a fake bucket and opens the store over it with files.NewGCS, the constructor a
// deployment runs, pointed at the fake.
func gcsStore(t *testing.T, bucket string) (*files.GCS, *filestest.GCS) {
	t.Helper()
	fake := filestest.ServeGCS(t, testBucket)
	store, err := files.NewGCS(context.Background(), bucket, fake.ClientOptions()...)
	if err != nil {
		t.Fatalf("NewGCS: %v", err)
	}
	return store, fake
}

func writes(requests []string) []string {
	var posts []string
	for _, request := range requests {
		if strings.HasPrefix(request, "POST ") {
			posts = append(posts, request)
		}
	}
	return posts
}

// The store needs storage.objects.create and storage.objects.get and nothing more: the whole
// round trip runs with only those two granted.
func TestGCSStoresAFileOnceByItsHashAndReadsItBack(t *testing.T) {
	store, fake := gcsStore(t, testBucket)
	fake.Grant("storage.objects.create", "storage.objects.get")
	ctx := context.Background()
	body := bytes.Repeat([]byte("dispatch "), 100_000)
	sha := files.SHA256(body)

	if err := store.Put(ctx, sha, "application/json; charset=utf-8", body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	mime, stored, held := fake.Object(files.Key(sha))
	if !held {
		t.Fatalf("Put stored nothing under %s", files.Key(sha))
	}
	if mime != "application/json" {
		t.Errorf("stored content type = %q, want the media type alone, application/json", mime)
	}
	if !bytes.Equal(stored, body) {
		t.Fatalf("stored %d bytes, want %d identical bytes", len(stored), len(body))
	}
	if posts := writes(fake.Requests()); len(posts) != 1 {
		t.Fatalf("the first Put wrote %d times, want once: %q", len(posts), posts)
	}

	// A second upload of the same bytes reads the key's metadata and writes nothing.
	before := len(fake.Requests())
	if err := store.Put(ctx, sha, "application/json", body); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if posts := writes(fake.Requests()[before:]); len(posts) != 0 {
		t.Errorf("second Put of held bytes wrote them again: %q", posts)
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

// Another writer storing the key between the store's metadata read and its write makes GCS
// refuse the write's ifGenerationMatch=0 with 412: the object is held, Put succeeds, and the
// object the other writer stored is not overwritten.
func TestGCSReadsAnObjectStoredInBetweenAsHeldAndNeverOverwritesIt(t *testing.T) {
	store, fake := gcsStore(t, testBucket)
	body := []byte("the bytes")
	sha := files.SHA256(body)
	fake.RaceNextUpload("text/plain", []byte("the other writer's bytes"))
	if err := store.Put(context.Background(), sha, "text/plain", body); err != nil {
		t.Fatalf("Put that lost the race: %v, want nil", err)
	}
	if _, stored, _ := fake.Object(files.Key(sha)); string(stored) != "the other writer's bytes" {
		t.Fatalf("the bucket holds %q, want the other writer's object left as it was", stored)
	}
}

func TestGCSRefusesABodyThatIsNotItsHashBeforeAnyCall(t *testing.T) {
	store, fake := gcsStore(t, testBucket)
	body := []byte("the bytes")
	wrong := files.SHA256([]byte("other bytes"))
	if err := store.Put(context.Background(), wrong, "text/plain", body); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("Put of a body under another body's hash: %v, want a refusal", err)
	}
	if requests := fake.Requests(); len(requests) != 0 {
		t.Fatalf("the refused Put reached the bucket: %q", requests)
	}
}

func TestGCSAnswersNotFoundForAHashItDoesNotHold(t *testing.T) {
	store, _ := gcsStore(t, testBucket)
	_, err := store.Get(context.Background(), strings.Repeat("ab", 32))
	if !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Get of an absent hash: %v, want ErrNotFound", err)
	}
}

// GCS answers a missing bucket 404 notFound, as it answers a missing object: the store reads it
// as a failure, never as an absent file, which the version route would answer 500 FILE_MISSING
// and a backfill would read as a lost file.
func TestGCSReadsAMissingBucketAsAFailureNotAnAbsence(t *testing.T) {
	store, fake := gcsStore(t, "no-such-bucket")
	ctx := context.Background()
	_, err := store.Get(ctx, strings.Repeat("ab", 32))
	if err == nil || errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Get from a missing bucket: %v, want a failure that is not ErrNotFound", err)
	}
	if err := store.Healthy(ctx); err == nil || !strings.Contains(err.Error(), "bucket does not exist") {
		t.Fatalf("Healthy of a missing bucket: %v, want a failure naming it", err)
	}
	if err := store.Put(ctx, files.SHA256([]byte("x")), "text/plain", []byte("x")); err == nil {
		t.Fatal("Put into a missing bucket answered nil")
	}
	if posts := writes(fake.Requests()); len(posts) != 0 {
		t.Fatalf("Put wrote past a missing bucket: %q", posts)
	}
}

// A caller with no grant on the bucket gets 403 for every call: a store failure, never an absent
// object, and Put writes nothing past it.
func TestGCSReadsForbiddenAsAFailureNotAnAbsence(t *testing.T) {
	store, fake := gcsStore(t, testBucket)
	fake.Grant()
	ctx := context.Background()
	sha := strings.Repeat("ab", 32)
	if _, err := store.Get(ctx, sha); err == nil || errors.Is(err, files.ErrNotFound) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Get under 403: %v, want a failure naming the refusal", err)
	}
	if err := store.Healthy(ctx); err == nil || !strings.Contains(err.Error(), "storage.objects.get") {
		t.Fatalf("Healthy under 403: %v, want a failure naming the missing permission", err)
	}
	if err := store.Put(ctx, files.SHA256([]byte("x")), "text/plain", []byte("x")); err == nil {
		t.Fatal("Put under 403 answered nil")
	}
	if posts := writes(fake.Requests()); len(posts) != 0 {
		t.Fatalf("Put wrote past a refused metadata read: %q", posts)
	}
}

// Healthy needs storage.objects.get alone: the probe's object does not exist, and GCS saying so
// is a healthy bucket. A grant without objects.create still fails the upload itself.
func TestGCSHealthyNeedsOnlyObjectsGet(t *testing.T) {
	store, fake := gcsStore(t, testBucket)
	fake.Grant("storage.objects.get")
	ctx := context.Background()
	if err := store.Healthy(ctx); err != nil {
		t.Fatalf("Healthy with storage.objects.get alone: %v, want nil", err)
	}
	if err := store.Put(ctx, files.SHA256([]byte("x")), "text/plain", []byte("x")); err == nil || !strings.Contains(err.Error(), "storage.objects.create") {
		t.Fatalf("Put without storage.objects.create: %v, want the refusal named", err)
	}
}

func TestGCSRefusesAnObjectPastTheSizeLimit(t *testing.T) {
	store, fake := gcsStore(t, testBucket)
	sha := strings.Repeat("cd", 32)
	fake.SetObject(files.Key(sha), "application/octet-stream", make([]byte, files.MaxObjectSize+1))
	if _, err := store.Get(context.Background(), sha); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Get of an oversize object: %v, want a size error", err)
	}
	if err := store.Put(context.Background(), sha, "application/octet-stream", make([]byte, files.MaxObjectSize+1)); err == nil {
		t.Fatal("Put accepted an oversize body")
	}
}

// An object whose bytes are not what its key says reads with an error before its end, and the
// reader hands out fewer bytes than the object holds before it says so.
func TestGCSRefusesABodyThatDoesNotHashToItsKey(t *testing.T) {
	store, fake := gcsStore(t, testBucket)
	same := bytes.Repeat([]byte("x"), 200_000)
	sha := files.SHA256(same)
	tampered := append(bytes.Repeat([]byte("x"), 199_999), 'y')
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

// Open reads gs://<bucket> as a Cloud Storage bucket and any other value as an S3 bucket's name,
// and refuses a gs:// value that names no bucket Cloud Storage could hold, with no store.
func TestOpenSelectsTheStoreBySetting(t *testing.T) {
	ctx := context.Background()
	// Application Default Credentials of their own, so the client finds a credential without the
	// developer's: construction reads the file and makes no call.
	dir := t.TempDir()
	credentials := filepath.Join(dir, "adc.json")
	if err := os.WriteFile(credentials, []byte(`{"type":"authorized_user","client_id":"test","client_secret":"test","refresh_token":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentials)
	t.Setenv("CLOUDSDK_CONFIG", dir)
	t.Setenv("GOOGLE_API_USE_CLIENT_CERTIFICATE", "false")
	filestest.PointSDKAt(t, "http://127.0.0.1:1")

	store, err := files.Open(ctx, "gs://example-files-bucket")
	if err != nil {
		t.Fatalf("Open(gs://example-files-bucket): %v", err)
	}
	if _, ok := store.(*files.GCS); !ok {
		t.Fatalf("Open(gs://example-files-bucket) = %T, want *files.GCS", store)
	}
	store, err = files.Open(ctx, "example-files-bucket")
	if err != nil {
		t.Fatalf("Open(example-files-bucket): %v", err)
	}
	if _, ok := store.(*files.S3); !ok {
		t.Fatalf("Open(example-files-bucket) = %T, want *files.S3", store)
	}

	for _, setting := range []string{
		"",
		"gs://",
		"gs://ab",
		"gs://Example-Files",
		"gs://example-files/prefix",
		"gs://-example-files",
		"gs://example-files-",
		"gs://example..files",
		"gs://192.168.5.4",
		"gs://goog-files",
		"gs://my-google-files",
		"gs://" + strings.Repeat("a", 64),
	} {
		store, err := files.Open(ctx, setting)
		if err == nil {
			t.Errorf("Open(%q) answered %T, want a refusal", setting, store)
			continue
		}
		if store != nil {
			t.Errorf("Open(%q) refused with a non-nil store %T", setting, store)
		}
	}
	// A dotted name may be longer than 63 characters while each part is at most 63.
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)
	if store, err := files.Open(ctx, "gs://"+long); err != nil {
		t.Errorf("Open(gs://%s): %v, want a GCS store", long, err)
	} else if _, ok := store.(*files.GCS); !ok {
		t.Errorf("Open(gs://%s) = %T, want *files.GCS", long, store)
	}
}
