package filestest

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/option"
)

// GCS is one bucket behind the Cloud Storage JSON API on loopback, enough of it for files.GCS:
// an object's metadata and media read, and a one-request multipart upload, with the error bodies
// GCS sends. As GCS does, it answers a missing object and a missing bucket alike, 404 notFound,
// told apart only by the message. It is stricter than GCS where the store's guarantees rest on
// what it sends: an upload is refused unless it is a single multipart request, carries
// ifGenerationMatch=0 (the store never overwrites) and holds the body's CRC32C and MD5 in its
// metadata, each matching the body, so a store that stopped sending any of them fails here rather
// than in production.
type GCS struct {
	mu       sync.Mutex
	url      string
	bucket   string
	objects  map[string]memoryObject
	requests []string
	// granted is the set of permissions the caller holds on the bucket; nil holds every one.
	granted map[string]bool
	// racer, when set, is stored under the next upload's name just before that upload is
	// handled: another writer that stored the key between the store's read and its write.
	racer *memoryObject
}

// ServeGCS starts a GCS holding the empty bucket, stopped when t ends. files.NewGCS(ctx, bucket,
// fake.ClientOptions()...) reaches it.
func ServeGCS(t testing.TB, bucket string) *GCS {
	t.Helper()
	fake := &GCS{bucket: bucket, objects: map[string]memoryObject{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	fake.url = server.URL
	return fake
}

// ClientOptions point the JSON API client at this bucket's server, with no credential.
func (f *GCS) ClientOptions() []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(f.url + "/storage/v1/"), option.WithoutAuthentication()}
}

// Grant limits the caller's permissions on the bucket to permissions
// ("storage.objects.get", "storage.objects.create"): a call needing any other answers 403, as GCS
// answers a principal its IAM policy does not grant. No call makes no permission held.
func (f *GCS) Grant(permissions ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.granted = map[string]bool{}
	for _, permission := range permissions {
		f.granted[permission] = true
	}
}

// Object is what the bucket holds under key.
func (f *GCS) Object(key string) (mime string, body []byte, held bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, held := f.objects[key]
	return object.mime, object.body, held
}

// SetObject stores body under key as it stands: an object another writer left, or a corrupted
// one.
func (f *GCS) SetObject(key, mime string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = memoryObject{mime: mime, body: body}
}

// RaceNextUpload stores body under the next upload's name just before the bucket handles that
// upload: a concurrent writer that won the key after the store found it absent.
func (f *GCS) RaceNextUpload(mime string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.racer = &memoryObject{mime: mime, body: body}
}

// Requests is every request the bucket has answered, as "<method> <decoded path>", in order.
func (f *GCS) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *GCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	query := r.URL.Query()
	media := query.Get("alt") == "media"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/"):
		bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/storage/v1/b/"), "/o/")
		if bucket != f.bucket {
			writeGCSError(w, media, http.StatusNotFound, "notFound", "The specified bucket does not exist.")
			return
		}
		if !f.allowed("storage.objects.get") {
			writeGCSError(w, media, http.StatusForbidden, "forbidden", "caller does not have storage.objects.get access to the Google Cloud Storage object. Permission 'storage.objects.get' denied on resource (or it may not exist).")
			return
		}
		object, held := f.objects[key]
		if !held {
			writeGCSError(w, media, http.StatusNotFound, "notFound", "No such object: "+bucket+"/"+key)
			return
		}
		if media {
			w.Header().Set("Content-Type", object.mime)
			w.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(object.body)
			return
		}
		writeGCSObject(w, bucket, key, object)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/upload/storage/v1/b/"):
		bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/upload/storage/v1/b/"), "/o")
		if bucket != f.bucket {
			writeGCSError(w, false, http.StatusNotFound, "notFound", "The specified bucket does not exist.")
			return
		}
		if !f.allowed("storage.objects.create") {
			writeGCSError(w, false, http.StatusForbidden, "forbidden", "caller does not have storage.objects.create access to the Google Cloud Storage object. Permission 'storage.objects.create' denied on resource (or it may not exist).")
			return
		}
		if query.Get("uploadType") != "multipart" {
			writeGCSError(w, false, http.StatusBadRequest, "invalid", "this bucket takes only one-request multipart uploads, whose metadata hashes GCS checks; got uploadType="+query.Get("uploadType"))
			return
		}
		if query.Get("ifGenerationMatch") != "0" {
			writeGCSError(w, false, http.StatusBadRequest, "invalid", "an upload must carry ifGenerationMatch=0, so it never overwrites an object")
			return
		}
		metadata, body, err := readMultipartUpload(r)
		if err != nil {
			writeGCSError(w, false, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		if err := checkUploadHashes(metadata, body); err != nil {
			writeGCSError(w, false, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		if f.racer != nil {
			f.objects[metadata.Name] = *f.racer
			f.racer = nil
		}
		if _, held := f.objects[metadata.Name]; held {
			writeGCSError(w, false, http.StatusPreconditionFailed, "conditionNotMet", "At least one of the pre-conditions you specified did not hold.")
			return
		}
		f.objects[metadata.Name] = memoryObject{mime: metadata.ContentType, body: body}
		writeGCSObject(w, bucket, metadata.Name, f.objects[metadata.Name])
	default:
		writeGCSError(w, false, http.StatusMethodNotAllowed, "invalid", r.Method+" "+r.URL.Path)
	}
}

func (f *GCS) allowed(permission string) bool {
	return f.granted == nil || f.granted[permission]
}

// uploadMetadata is the part of an object resource an upload's first part carries that the
// bucket reads.
type uploadMetadata struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Crc32c      string `json:"crc32c"`
	Md5Hash     string `json:"md5Hash"`
}

// readMultipartUpload reads a multipart/related upload: the object resource as JSON, then the
// media.
func readMultipartUpload(r *http.Request) (uploadMetadata, []byte, error) {
	var metadata uploadMetadata
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/related" {
		return metadata, nil, fmt.Errorf("an upload's content type must be multipart/related, got %q", r.Header.Get("Content-Type"))
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	first, err := reader.NextPart()
	if err != nil {
		return metadata, nil, fmt.Errorf("read the metadata part: %w", err)
	}
	if err := json.NewDecoder(first).Decode(&metadata); err != nil {
		return metadata, nil, fmt.Errorf("decode the metadata part: %w", err)
	}
	second, err := reader.NextPart()
	if err != nil {
		return metadata, nil, fmt.Errorf("read the media part: %w", err)
	}
	body, err := io.ReadAll(second)
	if err != nil {
		return metadata, nil, fmt.Errorf("read the media part: %w", err)
	}
	if metadata.Name == "" {
		return metadata, nil, fmt.Errorf("the metadata names no object")
	}
	return metadata, body, nil
}

// checkUploadHashes refuses an upload whose metadata does not carry the body's CRC32C and MD5, as
// GCS refuses one whose hashes do not match.
func checkUploadHashes(metadata uploadMetadata, body []byte) error {
	crc := make([]byte, 4)
	binary.BigEndian.PutUint32(crc, crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)))
	if want := base64.StdEncoding.EncodeToString(crc); metadata.Crc32c != want {
		return fmt.Errorf("Provided CRC32C %q doesn't match calculated CRC32C %q.", metadata.Crc32c, want)
	}
	sum := md5.Sum(body)
	if want := base64.StdEncoding.EncodeToString(sum[:]); metadata.Md5Hash != want {
		return fmt.Errorf("Provided MD5 hash %q doesn't match calculated MD5 hash %q.", metadata.Md5Hash, want)
	}
	return nil
}

func writeGCSObject(w http.ResponseWriter, bucket, key string, object memoryObject) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"kind":        "storage#object",
		"name":        key,
		"bucket":      bucket,
		"generation":  "1",
		"contentType": object.mime,
		"size":        strconv.Itoa(len(object.body)),
	})
}

// writeGCSError answers as GCS does: a JSON error resource, or, for a media download, the message
// alone as plain text.
func writeGCSError(w http.ResponseWriter, media bool, status int, reason, message string) {
	if media {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, message)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code":    status,
		"message": message,
		"errors":  []map[string]string{{"message": message, "domain": "global", "reason": reason}},
	}})
}
