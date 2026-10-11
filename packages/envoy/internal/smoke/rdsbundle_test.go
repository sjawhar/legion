//go:build smoke

package smoke

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// rdsBundleInImage is where the Envoy image ships the RDS CA bundle (docker/Dockerfile).
const rdsBundleInImage = "/etc/ssl/rds/global-bundle.pem"

// TestRDSBundle checks the RDS CA bundle in the image: the file at rdsBundleInImage is the one the
// repository vendors, byte for byte, and Go's system root pool, loaded inside the image as every
// binary there loads it, holds none of its certificates, while a pool made from the bundle holds
// all of them. A bundle copied under /etc/ssl/certs, which crypto/x509 reads file by file, would
// make every binary in the image trust each RDS CA for every TLS connection. The probe is a small
// static program (systemroots) built here for the image's architecture, which is this host's.
func TestRDSBundle(t *testing.T) {
	image := os.Getenv("ENVOY_SMOKE_IMAGE")
	if image == "" {
		t.Fatal("ENVOY_SMOKE_IMAGE is unset, and this test never builds the image itself; see TestSmoke for the build command.")
	}
	ctx := context.Background()

	probe := filepath.Join(t.TempDir(), "systemroots")
	build := exec.Command("go", "build", "-o", probe, "./systemroots")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the systemroots probe: %v\n%s", err, out)
	}

	container, err := testcontainers.Run(ctx, image,
		testcontainers.WithEntrypoint("sleep", "300"),
		testcontainers.WithFiles(testcontainers.ContainerFile{HostFilePath: probe, ContainerFilePath: "/tmp/systemroots", FileMode: 0o755}),
	)
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("start the image: %v", err)
	}

	shipped, err := container.CopyFileFromContainer(ctx, rdsBundleInImage)
	if err != nil {
		t.Fatalf("read %s from the image: %v", rdsBundleInImage, err)
	}
	defer shipped.Close()
	got, err := io.ReadAll(shipped)
	if err != nil {
		t.Fatalf("read %s from the image: %v", rdsBundleInImage, err)
	}
	want, err := os.ReadFile("../../docker/rds-global-bundle.pem")
	if err != nil {
		t.Fatalf("read the vendored bundle: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s in the image (%d bytes) is not docker/rds-global-bundle.pem (%d bytes)", rdsBundleInImage, len(got), len(want))
	}

	code, output, err := container.Exec(ctx, []string{"/tmp/systemroots", rdsBundleInImage}, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("run the systemroots probe in the image: %v", err)
	}
	printed, err := io.ReadAll(output)
	if err != nil {
		t.Fatalf("read the probe's output: %v", err)
	}
	report := strings.TrimSpace(string(printed))
	t.Logf("systemroots in the image: %s", report)
	if code != 0 {
		t.Fatalf("the systemroots probe exited %d: %s", code, report)
	}
	var system, bundle, total int
	if _, err := fmt.Sscanf(report, "system=%d bundle=%d total=%d", &system, &bundle, &total); err != nil {
		t.Fatalf("the probe printed %q: %v", report, err)
	}
	if total == 0 || bundle != total {
		t.Fatalf("a pool made from the bundle holds %d of its %d certificates; the probe cannot tell whether the system pool holds any", bundle, total)
	}
	if system != 0 {
		t.Fatalf("Go's system root pool in the image holds %d of the RDS bundle's %d certificates: every binary there trusts them for every TLS connection", system, total)
	}
}
