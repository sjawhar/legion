package filestest

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// S3 is one bucket behind S3's REST shape on loopback, enough of it for files.S3: HEAD and GET on
// the bucket and its objects, PUT, and the XML error bodies the SDK turns into typed errors. A HEAD
// of a missing key answers a bare 404 with no NoSuchKey, as S3 does. A PUT is refused unless its
// x-amz-checksum-sha256 header is the SHA-256 of its body: S3 refuses a body that does not match
// the checksum it was sent, and this fake also refuses one sent with none, so a store that stopped
// sending the hash fails here rather than in production.
type S3 struct {
	mu       sync.Mutex
	bucket   string
	objects  map[string]memoryObject
	requests []string
}

// ServeS3 starts an S3 holding the empty bucket and, for the rest of t, points this process's AWS
// SDK at it through the variables the SDK reads for itself: the endpoint, a region, static
// credentials, and no shared config or credentials file. files.NewS3(ctx, bucket) then reaches it
// exactly as a deployment's reaches its bucket. t.Setenv makes it unusable in a parallel test.
func ServeS3(t *testing.T, bucket string) *S3 {
	t.Helper()
	fake := &S3{bucket: bucket, objects: map[string]memoryObject{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	none := filepath.Join(t.TempDir(), "none")
	for name, value := range map[string]string{
		"AWS_ENDPOINT_URL_S3":                 server.URL,
		"AWS_ENDPOINT_URL":                    "",
		"AWS_IGNORE_CONFIGURED_ENDPOINT_URLS": "",
		"AWS_USE_DUALSTACK_ENDPOINT":          "",
		"AWS_USE_FIPS_ENDPOINT":               "",
		"AWS_REGION":                          "us-east-1",
		"AWS_DEFAULT_REGION":                  "",
		"AWS_ACCESS_KEY_ID":                   "test",
		"AWS_SECRET_ACCESS_KEY":               "test",
		"AWS_SESSION_TOKEN":                   "",
		"AWS_PROFILE":                         "",
		"AWS_DEFAULT_PROFILE":                 "",
		"AWS_CONFIG_FILE":                     none,
		"AWS_SHARED_CREDENTIALS_FILE":         none,
		"AWS_EC2_METADATA_DISABLED":           "true",
		"AWS_MAX_ATTEMPTS":                    "",
		"AWS_RETRY_MODE":                      "",
	} {
		t.Setenv(name, value)
	}
	return fake
}

// Object is what the bucket holds under key.
func (f *S3) Object(key string) (mime string, body []byte, held bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, held := f.objects[key]
	return object.mime, object.body, held
}

// SetObject stores body under key as it stands, its checksum unchecked: an object another writer
// left, or one the store would refuse to write.
func (f *S3) SetObject(key, mime string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = memoryObject{mime: mime, body: body}
}

// DeleteObject removes key: an object that went missing from the bucket.
func (f *S3) DeleteObject(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
}

// Requests is every request the bucket has answered, as "<method> <path>", in order.
func (f *S3) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *S3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	// Path style, /<bucket>/<key>: what the SDK sends to an endpoint whose host is an IP address.
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
		sum := sha256.Sum256(body)
		if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(sum[:]) {
			writeS3Error(w, http.StatusBadRequest, "BadDigest", "The SHA256 you specified did not match the calculated checksum.")
			return
		}
		f.objects[key] = memoryObject{mime: r.Header.Get("Content-Type"), body: body}
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
}
