package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/tests3"
)

// The store against a real S3-compatible server (tests3): the SDK's signing, its checksum
// header, the server's own 404 and NoSuchKey shapes and its checksum validation on read, none of
// which the loopback fake can vouch for. The backfill runs over it too, so a row's bytes make the
// whole trip: Postgres, the bucket, back through the verifying reader, and the row cleared.
func TestS3AgainstAnS3CompatibleServer(t *testing.T) {
	if os.Getenv("DISPATCH_TEST_NO_CONTAINERS") == "1" {
		t.Skip("DISPATCH_TEST_NO_CONTAINERS=1: this box runs no test containers")
	}
	const bucket = "example-files-bucket"
	tests3.Start(t, bucket)
	ctx := context.Background()
	store, err := files.NewS3(ctx, bucket)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if err := store.Healthy(ctx); err != nil {
		t.Fatalf("Healthy: %v", err)
	}

	body := bytes.Repeat([]byte("seaweed "), 400_000) // 3.2 MB, past one TCP window and the SDK's buffers
	sha := files.SHA256(body)
	if err := store.Put(ctx, sha, "application/octet-stream; charset=binary", body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(ctx, sha, "application/octet-stream", body); err != nil {
		t.Fatalf("second Put of the same bytes: %v", err)
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
	if _, err := store.Get(ctx, strings.Repeat("ab", 32)); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("Get of an absent hash: %v, want ErrNotFound", err)
	}
	// The server refuses a body that does not hash to the checksum the store sends with it.
	if err := store.Put(ctx, files.SHA256([]byte("other")), "text/plain", []byte("the bytes")); err == nil {
		t.Fatal("the server stored a body under a hash that is not its own")
	}

	// A row seeded as an upload before the store existed makes the whole trip and ends cleared,
	database := storetest.Open(t)
	id := seedFileVersion(t, database, "trip", []byte("the whole trip"))
	report, err := files.BackfillRows(ctx, database.Pool, store, &strings.Builder{})
	if err != nil || report.Done != 1 || len(report.Failed) != 0 || rowBytes(t, database, id) != nil {
		t.Fatalf("BackfillRows over the server: %+v, %v, row cleared %t", report, err, rowBytes(t, database, id) == nil)
	}
	if report, err := files.VerifyRows(ctx, database.Pool, store, &strings.Builder{}); err != nil || report.Done != 1 || len(report.Failed) != 0 {
		t.Fatalf("VerifyRows over the server: %+v, %v", report, err)
	}
	if report, err := files.RestoreRows(ctx, database.Pool, store, &strings.Builder{}); err != nil || report.Done != 1 || !bytes.Equal(rowBytes(t, database, id), []byte("the whole trip")) {
		t.Fatalf("RestoreRows over the server: %+v, %v", report, err)
	}
}
