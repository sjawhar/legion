// Package tests3 runs an S3-compatible server in a container for the tests that need a real one:
// the file store's own tests, which drive the AWS SDK's real client against it so what the store
// sends and how it reads the answers are exercised end to end, and the e2e harness when a run
// asks for one. SeaweedFS stands in for S3: MinIO's image left Docker Hub, and SeaweedFS answers
// the calls the store makes (HeadBucket, HeadObject, PutObject with a SHA-256 checksum, GetObject
// with checksum validation, and S3's own 404 and NoSuchKey shapes). A test that needs the fake's
// failure shapes uses files/filestest instead.
package tests3

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the SeaweedFS image every S3-compatible test container runs. The envoy-go CI job
// reads this declaration and pulls the image before its first test step, so no test reaches the
// registry mid-run; keep it a single-line string constant.
const Image = "chrislusf/seaweedfs:3.97"

// Start runs an S3-compatible server holding the empty bucket, removed when t ends, points this
// process's AWS SDK at it for the rest of t through the variables the SDK reads for itself (the
// endpoint, a region, static credentials, no shared config), so files.NewS3(ctx, bucket) reaches it
// exactly as a deployment's reaches its bucket, and returns the endpoint for a caller that hands it
// to another process. The server takes any credentials, so the static pair is only what the SDK
// needs to sign. t.Setenv makes it unusable in a parallel test.
func Start(t testing.TB, bucket string) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.Run(ctx, Image,
		testcontainers.WithCmd("server", "-s3", "-dir=/data", "-ip=0.0.0.0", "-s3.port=8333"),
		testcontainers.WithExposedPorts("8333/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/").WithPort("8333/tcp").WithStartupTimeout(90*time.Second)),
	)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("start the S3-compatible server: %v", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "8333/tcp", "http")
	if err != nil {
		t.Fatalf("S3-compatible server endpoint: %v", err)
	}
	// The server creates a bucket on an anonymous PUT of its name.
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"/"+bucket, nil)
	if err != nil {
		t.Fatalf("build the create-bucket request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create bucket %s: %s", bucket, response.Status)
	}
	none := filepath.Join(t.TempDir(), "none")
	for name, value := range map[string]string{
		"AWS_ENDPOINT_URL_S3":                 endpoint,
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
	return endpoint
}
