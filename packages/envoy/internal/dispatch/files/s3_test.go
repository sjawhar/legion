package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 is one bucket behind S3's REST shape on loopback, enough of it for the store: HEAD and
// GET on the bucket and its objects, PUT with the SHA-256 checksum header S3 verifies, and the
// XML error bodies the SDK turns into typed errors. The real client signs every request to it
// with static credentials, so what the store sends is what production sends.
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string]fakeObject
	// requests records "<method> <path>" in order, so a test can see what the store asked.
	requests []string
}

type fakeObject struct {
	mime string
	body []byte
}

func newFakeS3(bucket string) *fakeS3 {
	return &fakeS3{bucket: bucket, objects: map[string]fakeObject{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	// Path style: /<bucket>/<key>.
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	if key == "" {
		// HeadBucket.
		w.WriteHeader(http.StatusOK)
		return
	}
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		object, held := f.objects[key]
		if !held {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.Header().Set("Content-Type", object.mime)
		w.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(object.body)
		}
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "IncompleteBody", err.Error())
			return
		}
		// The SDK sends the body with aws-chunked encoding when it carries a trailing checksum;
		// decode it to the payload the way S3 does.
		if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
			body = decodeAWSChunked(body)
		}
		declared := r.Header.Get("X-Amz-Checksum-Sha256")
		if declared == "" {
			if trailer := r.Trailer.Get("X-Amz-Checksum-Sha256"); trailer != "" {
				declared = trailer
			}
		}
		if declared == "" {
			declared = trailerFromChunked(r)
		}
		sum := sha256.Sum256(body)
		if declared != base64.StdEncoding.EncodeToString(sum[:]) {
			writeS3Error(w, http.StatusBadRequest, "BadDigest", "The SHA256 you specified did not match the calculated checksum.")
			return
		}
		f.objects[key] = fakeObject{mime: r.Header.Get("Content-Type"), body: body}
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

// decodeAWSChunked strips aws-chunked framing: "<hex size>[;ext]\r\n<data>\r\n" chunks ending with
// a zero-size chunk, followed by trailers.
func decodeAWSChunked(raw []byte) []byte {
	var out []byte
	rest := raw
	for {
		line, after, found := bytes.Cut(rest, []byte("\r\n"))
		if !found {
			return out
		}
		sizeText, _, _ := strings.Cut(string(line), ";")
		var size int
		if _, err := fmt.Sscanf(sizeText, "%x", &size); err != nil || size == 0 {
			return out
		}
		if len(after) < size {
			return out
		}
		out = append(out, after[:size]...)
		rest = bytes.TrimPrefix(after[size:], []byte("\r\n"))
	}
}

// trailerFromChunked reads the checksum trailer an aws-chunked body ends with, when the request
// was not sent with HTTP trailers.
func trailerFromChunked(r *http.Request) string {
	return r.Header.Get("X-Amz-Trailer-Sha256")
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
}

// fakeStore starts a fakeS3 and returns a store over it through the real S3 client.
func fakeStore(t *testing.T) (*S3, *fakeS3) {
	t.Helper()
	const bucket = "dispatch-files"
	fake := newFakeS3(bucket)
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(server.URL),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	})
	return NewS3WithClient(client, bucket), fake
}

func TestS3StoresAFileOnceByItsHashAndReadsItBack(t *testing.T) {
	store, fake := fakeStore(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("dispatch "), 100_000)
	sha := SHA256(body)

	if err := store.Put(ctx, sha, "image/png", body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fake.mu.Lock()
	object, held := fake.objects[Key(sha)]
	fake.mu.Unlock()
	if !held {
		t.Fatalf("Put stored nothing under %s", Key(sha))
	}
	if object.mime != "image/png" {
		t.Errorf("stored content type = %q, want image/png", object.mime)
	}
	if !bytes.Equal(object.body, body) {
		t.Fatalf("stored %d bytes, want %d identical bytes", len(object.body), len(body))
	}

	// A second upload of the same bytes asks only whether the object is there and writes nothing.
	fake.mu.Lock()
	fake.requests = nil
	fake.mu.Unlock()
	if err := store.Put(ctx, sha, "image/png", body); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	fake.mu.Lock()
	requests := append([]string(nil), fake.requests...)
	fake.mu.Unlock()
	if len(requests) != 1 || !strings.HasPrefix(requests[0], "HEAD ") {
		t.Errorf("second Put sent %v, want one HEAD", requests)
	}

	got, err := store.Get(ctx, sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("Get read %d bytes, want %d identical bytes", len(got), len(body))
	}
	if err := store.Healthy(ctx); err != nil {
		t.Errorf("Healthy: %v", err)
	}
}

func TestS3AnswersNotFoundForAHashItDoesNotHold(t *testing.T) {
	store, _ := fakeStore(t)
	_, err := store.Get(context.Background(), strings.Repeat("ab", 32))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of an absent hash: %v, want ErrNotFound", err)
	}
}

func TestS3SendsTheHashAsTheObjectsChecksum(t *testing.T) {
	store, fake := fakeStore(t)
	ctx := context.Background()
	body := []byte("the bytes")
	wrong := SHA256([]byte("other bytes"))
	// The fake verifies the checksum header as S3 does, so a body stored under another body's
	// hash is refused by the bucket, not stored under a key that lies about its content.
	if err := store.Put(ctx, wrong, "text/plain", body); err == nil {
		t.Fatal("Put stored a body under a hash that is not its own")
	}
	fake.mu.Lock()
	_, held := fake.objects[Key(wrong)]
	fake.mu.Unlock()
	if held {
		t.Fatal("the refused object was stored")
	}
}

func TestS3RefusesAnObjectPastTheSizeLimit(t *testing.T) {
	store, fake := fakeStore(t)
	sha := strings.Repeat("cd", 32)
	fake.mu.Lock()
	fake.objects[Key(sha)] = fakeObject{mime: "application/octet-stream", body: make([]byte, MaxObjectSize+1)}
	fake.mu.Unlock()
	if _, err := store.Get(context.Background(), sha); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of an oversize object: %v, want a size error", err)
	}
	if err := store.Put(context.Background(), sha, "application/octet-stream", make([]byte, MaxObjectSize+1)); err == nil {
		t.Fatal("Put accepted an oversize body")
	}
}

func TestS3HealthyFailsForABucketThatDoesNotExist(t *testing.T) {
	store, _ := fakeStore(t)
	store.bucket = "no-such-bucket"
	if err := store.Healthy(context.Background()); err == nil {
		t.Fatal("Healthy answered nil for a bucket that does not exist")
	}
}
