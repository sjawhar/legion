package gcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The front door's certificate scripts (dispatch_frontdoor.go), run rather than read: each test
// renders a script as the Deployment does, for paths rooted in the test's own directories, and runs
// it on the shell tailnetTestShell names, against real certificates on disk and a stand-in API
// server holding Secret <hostname>-tailnet-tls. A missing tool fails the test rather than skipping
// it: without it nothing here judges the scripts.

const (
	frontDoorTestFQDN = "example-dispatch.example-tailnet.ts.net"
	frontDoorTestPod  = "example-dispatch-tailnet-"
)

// requireFrontDoorTools fails the test unless every tool the certificate scripts call is on PATH.
func requireFrontDoorTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "openssl", "base64", "date", "sed", "tr", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH; frontDoorToolsImage carries it, and this test fails rather than skips", tool)
		}
	}
}

// runFrontDoorScript runs script with env added, and returns what it wrote to stdout and stderr
// and how it exited.
func runFrontDoorScript(t *testing.T, script string, env ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out, errOut strings.Builder
	cmd := tailnetTestShell(ctx, "-c", script)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the script did not finish:\n%s\n%s", out.String(), errOut.String())
	}
	return out.String(), errOut.String(), err
}

// writeFrontDoorCert writes a self-signed certificate for name, expiring at notAfter, as
// <dir>/<base>.crt and <base>.key, and returns its PEM.
func writeFrontDoorCert(t *testing.T, dir, base, name string, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, base+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, base+".crt"), []byte(certPEM), 0o644))
	return certPEM
}

// newFrontDoorCertPaths roots a dispatchCertPaths in the test's own directories, with tailscaled's
// certificate cache created as tailscaled creates it, and a token for the scripts to present.
func newFrontDoorCertPaths(t *testing.T, secretURL string) dispatchCertPaths {
	t.Helper()
	p := dispatchCertPaths{service: tailnetServicePaths{mount: t.TempDir()}, secret: t.TempDir(), tokens: t.TempDir(), secretURL: secretURL}
	require.NoError(t, os.MkdirAll(p.cache(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p.tokens, "token"), []byte("not-a-real-token"), 0o600))
	return p
}

// The pod's grace period holds the tailscale container's preStop hook and then containerboot's own
// shutdown, so a replaced pod's node drains and logs out before the kubelet kills it.
func TestDispatchFrontDoorGraceCoversTheShutdown(t *testing.T) {
	require.GreaterOrEqual(t, dispatchFrontDoorGraceSeconds, tailnetShutdownSeconds)
}

// cert-seed copies a published certificate, its key and the ACME account key into tailscaled's
// cache, owner-only, before tailscaled starts, and creates the state directory as the pod's uid.
// The negative controls: a Secret with any of the three keys missing or empty seeds nothing, and
// the pod starts with an empty cache and no error.
func TestDispatchCertSeedSeedsTheCacheFromThePublishedSecret(t *testing.T) {
	requireFrontDoorTools(t)
	p := dispatchCertPaths{service: tailnetServicePaths{mount: t.TempDir()}, secret: t.TempDir()}
	published := writeFrontDoorCert(t, p.secret, "tls", frontDoorTestFQDN, time.Now().Add(60*24*time.Hour))
	require.NoError(t, os.WriteFile(filepath.Join(p.secret, "acme-account.key.pem"), []byte("account key"), 0o644))

	stdout, stderr, err := runFrontDoorScript(t, dispatchCertSeedScript(p, frontDoorTestFQDN))
	require.NoError(t, err, stderr)
	require.Contains(t, stdout, "seeded the cached certificate for "+frontDoorTestFQDN)
	info, err := os.Stat(p.service.stateDir())
	require.NoError(t, err, "cert-seed creates the state directory")
	require.True(t, info.IsDir())
	info, err = os.Stat(p.cache())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "the cache is searchable by the pod's uid alone")
	for file, mode := range map[string]os.FileMode{frontDoorTestFQDN + ".crt": 0o644, frontDoorTestFQDN + ".key": 0o600, "acme-account.key.pem": 0o600} {
		info, err := os.Stat(filepath.Join(p.cache(), file))
		require.NoError(t, err, file)
		require.Equal(t, mode, info.Mode().Perm(), file)
	}
	cached, err := os.ReadFile(filepath.Join(p.cache(), frontDoorTestFQDN+".crt"))
	require.NoError(t, err)
	require.Equal(t, published, string(cached))

	for _, missing := range []string{"tls.crt", "tls.key", "acme-account.key.pem"} {
		t.Run("without "+missing, func(t *testing.T) {
			q := dispatchCertPaths{service: tailnetServicePaths{mount: t.TempDir()}, secret: t.TempDir()}
			writeFrontDoorCert(t, q.secret, "tls", frontDoorTestFQDN, time.Now().Add(60*24*time.Hour))
			require.NoError(t, os.WriteFile(filepath.Join(q.secret, "acme-account.key.pem"), []byte("account key"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(q.secret, missing), nil, 0o644), "the kubelet mounts a key with no value as an empty file")
			stdout, stderr, err := runFrontDoorScript(t, dispatchCertSeedScript(q, frontDoorTestFQDN))
			require.NoError(t, err, "an unpublished certificate is no error: %s", stderr)
			require.Contains(t, stdout, "no published certificate in "+q.secret)
			entries, err := os.ReadDir(q.cache())
			require.NoError(t, err, "the cache exists for tailscaled")
			require.Empty(t, entries, "nothing is seeded")
		})
	}
}

// frontDoorSecretStore is the one Kubernetes Secret every front-door pod's publisher patches,
// behind a server that applies each merge patch to it, writes it where the kubelet's mount shows
// it, and records what was written.
type frontDoorSecretStore struct {
	dir    string
	url    string
	mu     sync.Mutex
	writes []map[string]string
}

func newFrontDoorSecretStore(t *testing.T) *frontDoorSecretStore {
	t.Helper()
	s := &frontDoorSecretStore{dir: t.TempDir()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "want PATCH, got "+r.Method, http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Content-Type"); got != "application/merge-patch+json" {
			http.Error(w, "want a merge patch, got "+got, http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var patch struct{ Data map[string]string }
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.writes = append(s.writes, patch.Data)
		for key, encoded := range patch.Data {
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := os.WriteFile(filepath.Join(s.dir, key), raw, 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	s.url = server.URL + "/api/v1/namespaces/dispatch/secrets/example-dispatch-tailnet-tls"
	return s
}

func (s *frontDoorSecretStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

// hold puts a certificate in the Secret, as the kubelet's mount shows it.
func (s *frontDoorSecretStore) hold(t *testing.T, certPEM string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(s.dir, "tls.crt"), []byte(certPEM), 0o600))
}

// published is the certificate in the Secret now, or "" when it holds none.
func (s *frontDoorSecretStore) published(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, "tls.crt"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)
	return string(b)
}

// publishPod is one front-door pod's publisher: its own tailscaled cache and one pass of the loop.
type publishPod struct {
	paths  dispatchCertPaths
	script string
}

// newPublishPod renders dispatchCertPublishScript for a pod writing into store, with its endless
// loop replaced by one publish so a test drives one pass. The substitution must bite, or the test
// would hang on the loop, so the tail is asserted first.
func newPublishPod(t *testing.T, store *frontDoorSecretStore) *publishPod {
	t.Helper()
	requireFrontDoorTools(t)
	paths := newFrontDoorCertPaths(t, store.url)
	paths.secret = store.dir
	script := dispatchCertPublishScript(paths, frontDoorTestFQDN)
	const loop = "while true; do\n  publish\n  sleep 60\ndone\n"
	require.True(t, strings.HasSuffix(script, loop), "the publisher ends with the loop this test replaces:\n%s", script)
	return &publishPod{paths: paths, script: strings.TrimSuffix(script, loop) + "publish\n"}
}

// holds puts a certificate for name, expiring at notAfter, in this pod's cache under the Service
// name's file, with the ACME account key beside it.
func (p *publishPod) holds(t *testing.T, name string, notAfter time.Time) {
	t.Helper()
	writeFrontDoorCert(t, p.paths.cache(), frontDoorTestFQDN, name, notAfter)
	require.NoError(t, os.WriteFile(filepath.Join(p.paths.cache(), "acme-account.key.pem"), []byte("account key"), 0o600))
}

// publish runs one pass with env added. The loop keeps going past a failure by design, so a
// non-zero exit is the test's problem and not the behaviour under test.
func (p *publishPod) publish(t *testing.T, env ...string) {
	t.Helper()
	stdout, stderr, err := runFrontDoorScript(t, p.script, env...)
	require.NoError(t, err, "one pass of the publisher: %s %s", stdout, stderr)
}

// run runs passes passes in one shell, as the container does.
func (p *publishPod) run(t *testing.T, passes int) (stdout, stderr string, err error) {
	t.Helper()
	return runFrontDoorScript(t, p.script+strings.Repeat("publish\n", passes-1))
}

// A pod publishes its certificate only when it is later than the published one: a later one
// replaces the Secret's, an earlier one leaves it. An order, not a difference, so pods holding
// different certificates do not overwrite each other for ever.
func TestDispatchCertPublishWritesOnlyALaterCertificate(t *testing.T) {
	store := newFrontDoorSecretStore(t)
	later := writeFrontDoorCert(t, t.TempDir(), "later", frontDoorTestFQDN, time.Now().Add(80*24*time.Hour))
	store.hold(t, later)

	earlier := newPublishPod(t, store)
	earlier.holds(t, frontDoorTestFQDN, time.Now().Add(40*24*time.Hour))
	earlier.publish(t)
	require.Zero(t, store.count(), "a pod whose certificate expires before the published one writes nothing")
	require.Equal(t, later, store.published(t))

	latest := newPublishPod(t, store)
	latest.holds(t, frontDoorTestFQDN, time.Now().Add(85*24*time.Hour))
	latest.publish(t)
	require.Equal(t, 1, store.count(), "a pod whose certificate expires after the published one writes it")
	require.NotEqual(t, later, store.published(t))
	latest.publish(t)
	require.Equal(t, 1, store.count(), "and then goes quiet: the published certificate is its own")
}

// Two pods whose certificates expire at the same instant converge. The comparison is -le for
// exactly this case: with -lt neither pod would see the other's as later or equal, and both would
// write every pass for ever.
func TestDispatchCertPublishConvergesOnAnEqualExpiry(t *testing.T) {
	store := newFrontDoorSecretStore(t)
	expiry := time.Now().Add(60 * 24 * time.Hour).Truncate(time.Second)
	a, b := newPublishPod(t, store), newPublishPod(t, store)
	a.holds(t, frontDoorTestFQDN, expiry)
	b.holds(t, frontDoorTestFQDN, expiry)

	a.publish(t)
	require.Equal(t, 1, store.count(), "pod A writes into an empty Secret")
	for pass := range 3 {
		b.publish(t)
		a.publish(t)
		require.Equal(t, 1, store.count(), "after pass %d: pod B's certificate expires at the same instant as the published one, so neither pod writes again", pass+1)
	}
}

// A certificate in the Secret for another name is replaced, however long it has left: the order
// the loop decides by holds only between certificates for one name.
func TestDispatchCertPublishReplacesACertificateForAnotherName(t *testing.T) {
	store := newFrontDoorSecretStore(t)
	stale := writeFrontDoorCert(t, t.TempDir(), "stale", "old-name.example-tailnet.ts.net", time.Now().Add(300*24*time.Hour))
	store.hold(t, stale)

	p := newPublishPod(t, store)
	p.holds(t, frontDoorTestFQDN, time.Now().Add(30*24*time.Hour))
	p.publish(t)
	require.Equal(t, 1, store.count(), "a certificate for another name is not comparable with this pod's and is replaced")
	require.NotEqual(t, stale, store.published(t))
	p.publish(t)
	require.Equal(t, 1, store.count(), "the pod does not rewrite its own certificate")
}

// The name match reads the subjectAltName and matches it whole: a name that only begins with the
// Service's, or a wildcard over it, is no match, so the published certificate is replaced.
func TestDispatchCertPublishMatchesTheNameExactly(t *testing.T) {
	for _, name := range []string{frontDoorTestFQDN + ".other.example", "*.example-tailnet.ts.net", "x" + frontDoorTestFQDN} {
		t.Run(name, func(t *testing.T) {
			store := newFrontDoorSecretStore(t)
			store.hold(t, writeFrontDoorCert(t, t.TempDir(), "near", name, time.Now().Add(300*24*time.Hour)))
			p := newPublishPod(t, store)
			p.holds(t, frontDoorTestFQDN, time.Now().Add(30*24*time.Hour))
			p.publish(t)
			require.Equal(t, 1, store.count(), "a certificate for %s is not one for %s", name, frontDoorTestFQDN)
		})
	}
}

// The name check must not rest on openssl's exit status: on the OpenSSL 3.0 releases images carry,
// `openssl x509 -checkhost` over a certificate for another name prints its verdict and exits 0.
// This drives the publisher with an openssl that answers -checkhost that way.
func TestDispatchCertPublishReplacesACertificateForAnotherNameWhenCheckhostExitsZero(t *testing.T) {
	store := newFrontDoorSecretStore(t)
	stale := writeFrontDoorCert(t, t.TempDir(), "stale", "old-name.example-tailnet.ts.net", time.Now().Add(300*24*time.Hour))
	store.hold(t, stale)
	real, err := exec.LookPath("openssl")
	require.NoError(t, err)
	shim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(shim, "openssl"), []byte(fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "-checkhost" ]; then
    %[1]q "$@"
    exit 0
  fi
done
exec %[1]q "$@"
`, real)), 0o755))

	p := newPublishPod(t, store)
	p.holds(t, frontDoorTestFQDN, time.Now().Add(30*24*time.Hour))
	p.publish(t, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.Equal(t, 1, store.count(), "with an openssl whose -checkhost exits 0 either way, a certificate for another name is still replaced")
	require.NotEqual(t, stale, store.published(t))
}

// A cache this container's identity cannot read ends the container, naming the path and its uid:
// skipping the pass instead would leave the Secret empty with nothing saying why, and an identity
// that cannot read the cache now never will.
func TestDispatchCertPublishExitsWhenItCannotReadTheCache(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as euid 0, which permission bits do not restrict, so no path in the cache can be made unreadable")
	}
	for _, c := range []struct {
		name string
		path func(p *publishPod) string
	}{
		{"certs directory", func(p *publishPod) string { return p.paths.cache() }},
		{"key", func(p *publishPod) string { return filepath.Join(p.paths.cache(), frontDoorTestFQDN+".key") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newFrontDoorSecretStore(t)
			p := newPublishPod(t, store)
			p.holds(t, frontDoorTestFQDN, time.Now().Add(30*24*time.Hour))
			path := c.path(p)
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.NoError(t, os.Chmod(path, 0))
			t.Cleanup(func() { _ = os.Chmod(path, info.Mode().Perm()) })

			_, stderr, err := p.run(t, 1)
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, "one pass over an unreadable %s exits non-zero (stderr: %q)", c.name, stderr)
			require.Contains(t, stderr, path)
			require.Contains(t, stderr, "uid "+strconv.Itoa(os.Getuid()))
			require.Zero(t, store.count())
		})
	}
}

// Until tailscaled's first order the cache is empty, which is normal: the pass writes nothing,
// exits 0, and says once, not every 60 s, what it waits for.
func TestDispatchCertPublishSaysOnceThatItIsWaitingForACertificate(t *testing.T) {
	store := newFrontDoorSecretStore(t)
	p := newPublishPod(t, store)
	stdout, stderr, err := p.run(t, 2)
	require.NoError(t, err, stderr)
	require.Equal(t, 1, strings.Count(stdout, "waiting for tailscaled to cache a certificate under "+p.paths.cache()), stdout)
	require.Zero(t, store.count())
}

// frontDoorLedger is Secret <hostname>-tailnet-tls's claim annotation, its node registry and its
// resourceVersion, behind a server that applies a merge patch only when its resourceVersion is the
// current one, as the API server does, and changes only the annotations the patch names. It
// answers a GET only as the metadata-only PartialObjectMetadata the gate asks for
// (partialMetadataAccept), and fails the test on a GET that does not ask for it, which would read
// the Secret's private key.
type frontDoorLedger struct {
	mu                  sync.Mutex
	rv                  int
	claims, nodes       string
	requests, writes    int
	claimWrites         int
	conflicts           int
	conflictOnce        bool // moves the resourceVersion between the next read and its write
	url                 string
	annotationsReceived []map[string]string
}

func newFrontDoorLedger(t *testing.T, claims string) *frontDoorLedger {
	t.Helper()
	l := &frontDoorLedger{rv: 7, claims: claims}
	fields := func() map[string]*string {
		return map[string]*string{dispatchCertificateClaimsAnnotation: &l.claims, dispatchFrontDoorNodesAnnotation: &l.nodes}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.requests++
		switch r.Method {
		case http.MethodGet:
			if got := r.Header.Get("Accept"); got != partialMetadataAccept {
				t.Errorf("the gate's GET sent Accept %q, want %q: it must read the Secret's metadata alone", got, partialMetadataAccept)
				http.Error(w, "want PartialObjectMetadata", http.StatusNotAcceptable)
				return
			}
			annotations := map[string]string{}
			for key, field := range fields() {
				if *field != "" {
					annotations[key] = *field
				}
			}
			// A PartialObjectMetadata, its fields in the API server's order, indented as it
			// answers curl, with no data and with the managed fields naming the annotation keys,
			// which the gate must not take for the annotations themselves.
			body, _ := json.MarshalIndent(struct {
				Kind       string `json:"kind"`
				APIVersion string `json:"apiVersion"`
				Metadata   any    `json:"metadata"`
			}{"PartialObjectMetadata", "meta.k8s.io/v1", struct {
				Name              string            `json:"name"`
				Namespace         string            `json:"namespace"`
				UID               string            `json:"uid"`
				ResourceVersion   string            `json:"resourceVersion"`
				CreationTimestamp string            `json:"creationTimestamp"`
				Annotations       map[string]string `json:"annotations,omitempty"`
				ManagedFields     []any             `json:"managedFields"`
			}{
				"example-dispatch-tailnet-tls", "dispatch", "00000000-0000-0000-0000-000000000000", strconv.Itoa(l.rv), "2026-01-01T00:00:00Z", annotations,
				[]any{map[string]any{"fieldsV1": map[string]any{"f:metadata": map[string]any{"f:annotations": map[string]any{
					"f:" + dispatchCertificateClaimsAnnotation: map[string]any{}, "f:" + dispatchFrontDoorNodesAnnotation: map[string]any{},
				}}}}},
			}}, "", "  ")
			if l.conflictOnce {
				l.conflictOnce = false
				l.rv++
			}
			_, _ = w.Write(body)
		case http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				http.Error(w, "want a merge patch", http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var patch struct {
				Metadata struct {
					ResourceVersion string            `json:"resourceVersion"`
					Annotations     map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(body, &patch); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if patch.Metadata.ResourceVersion != strconv.Itoa(l.rv) {
				l.conflicts++
				http.Error(w, `{"reason":"Conflict"}`, http.StatusConflict)
				return
			}
			l.writes++
			l.annotationsReceived = append(l.annotationsReceived, patch.Metadata.Annotations)
			for key, field := range fields() {
				value, ok := patch.Metadata.Annotations[key]
				if !ok {
					continue
				}
				if key == dispatchCertificateClaimsAnnotation && value != l.claims {
					l.claimWrites++
				}
				*field = value
			}
			l.rv++
			_, _ = w.Write([]byte("{}"))
		default:
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	l.url = server.URL + "/api/v1/namespaces/dispatch/secrets/example-dispatch-tailnet-tls"
	return l
}

// partialMetadataAccept is the Accept header that has the API server answer a GET with the
// object's PartialObjectMetadata, its metadata alone.
const partialMetadataAccept = "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1"

// gatePod is one front-door pod's view for the claim gate: its paths, rooted in the test's own
// directories, and the files the tests read and write, taken from them.
type gatePod struct {
	name, flag, nodeIDFile string
	paths                  dispatchCertPaths
}

func newGatePod(t *testing.T, name string) *gatePod {
	t.Helper()
	requireFrontDoorTools(t)
	paths := newFrontDoorCertPaths(t, "")
	p := &gatePod{name: name, paths: paths, flag: paths.service.advertiseFlag(), nodeIDFile: paths.service.nodeID()}
	require.NoError(t, os.WriteFile(p.nodeIDFile, []byte(gateNodeID(name)+"\n"), 0o644))
	return p
}

// gateNodeID is the node id a test pod's own tailscaled reports.
func gateNodeID(pod string) string { return "n" + strings.TrimPrefix(pod, frontDoorTestPod) + "CNTRL" }

// gate renders dispatchCertClaimScript for p against l with its retry loop replaced by one pass,
// runs it, and returns whether the pass let the pod advertise, and what it printed. The
// substitution must bite, or the test would hang on the loop, so the tail is asserted first.
func (p *gatePod) gate(t *testing.T, l *frontDoorLedger) (bool, string) {
	t.Helper()
	paths := p.paths
	paths.secretURL = l.url
	script := dispatchCertClaimScript(paths, frontDoorTestFQDN)
	const tail = "  while :; do gate; sleep 30; done\n) &\n"
	require.True(t, strings.HasSuffix(script, tail), "the claim gate ends with the loop this test replaces:\n%s", script)
	stdout, stderr, err := runFrontDoorScript(t, strings.TrimSuffix(script, tail)+"  gate\n)\n", "HOSTNAME="+p.name)
	_, flagErr := os.Stat(p.flag)
	require.Equal(t, err == nil, flagErr == nil, "the gate exited %v but the flag's presence is %v:\n%s%s", err, flagErr == nil, stdout, stderr)
	return err == nil, stdout + stderr
}

// registeredIn reports whether the ledger's registry names p's own current node, and only once.
func registeredIn(l *frontDoorLedger, p *gatePod) bool {
	n := 0
	for _, e := range strings.Fields(l.nodes) {
		parts := strings.Split(e, "/")
		if len(parts) == 3 && parts[1] == p.name && parts[2] == gateNodeID(p.name) {
			n++
		}
	}
	return n == 1
}

func gateClaimsAt(ages ...time.Duration) string {
	var out []string
	for i, age := range ages {
		out = append(out, fmt.Sprintf("%d/%sother%d", time.Now().Add(-age).Unix(), frontDoorTestPod, i))
	}
	return strings.Join(out, " ")
}

// With nothing cached or published, a pod advertises only by taking a claim, and takes it in the
// same write that records its node; with every claim of the week taken it does not advertise and
// writes nothing (the negative control). A restarted container of the pod holding a claim
// advertises again without another.
func TestDispatchCertificateBudgetClaims(t *testing.T) {
	l := newFrontDoorLedger(t, gateClaimsAt(time.Hour, 2*time.Hour))
	p := newGatePod(t, frontDoorTestPod+"a")
	ok, out := p.gate(t, l)
	require.True(t, ok, out)
	require.Equal(t, 1, l.writes, out)
	require.Equal(t, 1, l.claimWrites, out)
	require.Contains(t, l.claims, "/"+p.name)
	require.Len(t, strings.Fields(l.claims), dispatchCertificateClaimsPerWeek)
	require.True(t, registeredIn(l, p), l.nodes)

	q := newGatePod(t, frontDoorTestPod+"b")
	ok, out = q.gate(t, l)
	require.False(t, ok, out)
	require.Equal(t, 1, l.writes, "a pod past the budget writes nothing")
	require.Contains(t, out, "no claim left")
	require.False(t, registeredIn(l, q))

	require.NoError(t, os.Remove(p.flag))
	ok, out = p.gate(t, l)
	require.True(t, ok, out)
	require.Equal(t, 1, l.claimWrites, out)
	require.Contains(t, out, "this pod holds claim")
	require.True(t, registeredIn(l, p))
}

// A claim older than the claim window no longer counts, and is dropped when the ledger is next
// written. The negative control is TestDispatchCertificateBudgetClaims's full week.
func TestDispatchCertificateBudgetForgetsOldClaims(t *testing.T) {
	l := newFrontDoorLedger(t, gateClaimsAt(time.Hour, 2*time.Hour, 8*24*time.Hour))
	p := newGatePod(t, frontDoorTestPod+"a")
	ok, out := p.gate(t, l)
	require.True(t, ok, out)
	require.Len(t, strings.Fields(l.claims), dispatchCertificateClaimsPerWeek, l.claims)
	require.NotContains(t, l.claims, "other2")
}

// The registry is not the claims: another pod's write keeps every entry younger than the keep
// window, so a pod gated before its claim lapsed stays named, and the writer's own entry for an
// older node id stays too. The negative control: an entry older than the keep window is dropped.
func TestDispatchCertificateBudgetRegistryOutlivesTheClaims(t *testing.T) {
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	ancient := time.Now().Add(-31 * 24 * time.Hour).Unix()
	l := newFrontDoorLedger(t, fmt.Sprintf("%d/%sb", old, frontDoorTestPod))
	l.nodes = fmt.Sprintf("%d/%sb/nbCNTRL %d/%sgone/ngoneCNTRL %d/%sa/nSTALE", old, frontDoorTestPod, ancient, frontDoorTestPod, old, frontDoorTestPod)
	p := newGatePod(t, frontDoorTestPod+"a")
	ok, out := p.gate(t, l)
	require.True(t, ok, out)
	require.Contains(t, l.nodes, "/"+frontDoorTestPod+"b/nbCNTRL")
	require.NotContains(t, l.nodes, "ngoneCNTRL")
	require.Contains(t, l.nodes, "/"+frontDoorTestPod+"a/nSTALE")
	require.True(t, registeredIn(l, p), l.nodes)
}

// Two pods cannot take one claim: a write against a resourceVersion another write has moved is
// refused, the pod does not advertise, and its next pass reads the ledger again.
func TestDispatchCertificateBudgetClaimNeedsTheCurrentLedger(t *testing.T) {
	l := newFrontDoorLedger(t, gateClaimsAt(time.Hour, 2*time.Hour))
	l.conflictOnce = true
	p := newGatePod(t, frontDoorTestPod+"a")
	ok, out := p.gate(t, l)
	require.False(t, ok, out)
	require.Zero(t, l.writes)
	require.Equal(t, 1, l.conflicts)
	require.Contains(t, out, "the write was refused")
	ok, out = p.gate(t, l)
	require.True(t, ok, out)
	require.Equal(t, 1, l.writes)
}

// A pod whose cache holds the certificate with more than 31 days left advertises with no claim: it
// records its node and orders nothing until the certificate falls due. One with less left must
// claim, which, with every claim taken, it cannot: the negative control.
func TestDispatchCertificateBudgetCachedCertificateSpendsNothing(t *testing.T) {
	l := newFrontDoorLedger(t, gateClaimsAt(time.Hour, 2*time.Hour, 3*time.Hour))
	p := newGatePod(t, frontDoorTestPod+"a")
	writeFrontDoorCert(t, p.paths.cache(), frontDoorTestFQDN, frontDoorTestFQDN, time.Now().Add(60*24*time.Hour))
	ok, out := p.gate(t, l)
	require.True(t, ok, out)
	require.Zero(t, l.claimWrites)
	require.True(t, registeredIn(l, p), l.nodes)
	require.Contains(t, out, "the cache holds the certificate")

	q := newGatePod(t, frontDoorTestPod+"b")
	writeFrontDoorCert(t, q.paths.cache(), frontDoorTestFQDN, frontDoorTestFQDN, time.Now().Add(20*24*time.Hour))
	ok, out = q.gate(t, l)
	require.False(t, ok, "a certificate due for renewal buys no advertisement without a claim:\n%s", out)
	require.False(t, registeredIn(l, q))
}

// A pod whose cache is empty but whose mounted Secret holds the certificate (a pod started before
// a publisher wrote it) copies it into the cache, owner-only for the key, and advertises with no
// claim. The negative controls: a published certificate for another name, and one for this name
// with 31 days or less left, are not copied and buy nothing, with every claim taken.
func TestDispatchCertificateBudgetCopiesThePublishedCertificate(t *testing.T) {
	l := newFrontDoorLedger(t, gateClaimsAt(time.Hour, 2*time.Hour, 3*time.Hour))
	p := newGatePod(t, frontDoorTestPod+"a")
	writeFrontDoorCert(t, p.paths.secret, "tls", frontDoorTestFQDN, time.Now().Add(60*24*time.Hour))
	require.NoError(t, os.WriteFile(filepath.Join(p.paths.secret, "acme-account.key.pem"), []byte("account key"), 0o644))
	ok, out := p.gate(t, l)
	require.True(t, ok, out)
	require.Zero(t, l.claimWrites)
	require.True(t, registeredIn(l, p), l.nodes)
	for file, mode := range map[string]os.FileMode{frontDoorTestFQDN + ".crt": 0o644, frontDoorTestFQDN + ".key": 0o600, "acme-account.key.pem": 0o600} {
		info, err := os.Stat(filepath.Join(p.paths.cache(), file))
		require.NoError(t, err, "%s is copied into the cache", file)
		require.Equal(t, mode, info.Mode().Perm(), file)
	}

	for name, c := range map[string]struct {
		fqdn string
		life time.Duration
	}{
		"another name":       {"old-name.example-tailnet.ts.net", 60 * 24 * time.Hour},
		"this name, 20 days": {frontDoorTestFQDN, 20 * 24 * time.Hour},
		"this name, 31 days": {frontDoorTestFQDN, 31*24*time.Hour - time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			q := newGatePod(t, frontDoorTestPod+"b")
			writeFrontDoorCert(t, q.paths.secret, "tls", c.fqdn, time.Now().Add(c.life))
			ok, out := q.gate(t, l)
			require.False(t, ok, "a published certificate for %s let the pod advertise:\n%s", name, out)
			_, err := os.Stat(filepath.Join(q.paths.cache(), frontDoorTestFQDN+".crt"))
			require.ErrorIs(t, err, os.ErrNotExist, "a published certificate for %s was copied into the cache", name)
		})
	}
}

// The gate asks nothing and sets no flag before the advertiser has read this pod's own node id: a
// pod the registry cannot name never advertises. The negative control: with the id, it does.
func TestDispatchCertificateBudgetNeedsThePodsOwnNodeID(t *testing.T) {
	l := newFrontDoorLedger(t, gateClaimsAt(time.Hour))
	p := newGatePod(t, frontDoorTestPod+"a")
	require.NoError(t, os.Remove(p.nodeIDFile))
	ok, out := p.gate(t, l)
	require.False(t, ok, out)
	require.Zero(t, l.requests)
	require.Contains(t, out, "node id is not read yet")
	require.NoError(t, os.WriteFile(p.nodeIDFile, []byte(gateNodeID(p.name)+"\n"), 0o644))
	ok, out = p.gate(t, l)
	require.True(t, ok, out)
	require.True(t, registeredIn(l, p), l.nodes)
}

// The gate keeps the registry naming this pod's current node: a pass with the flag set and its
// entry in place writes nothing (the negative control), one whose entry is gone writes it again,
// and one whose node id file names a new node records the new id and puts it first in the flag,
// all without a claim, keeping the pod's entry for its old node id.
func TestDispatchCertificateBudgetReRegisters(t *testing.T) {
	l := newFrontDoorLedger(t, gateClaimsAt(time.Hour))
	p := newGatePod(t, frontDoorTestPod+"a")
	ok, out := p.gate(t, l)
	require.True(t, ok, out)
	require.True(t, registeredIn(l, p), l.nodes)
	writes, claims := l.writes, l.claimWrites

	ok, out = p.gate(t, l)
	require.True(t, ok, out)
	require.Equal(t, writes, l.writes, "a pass with the entry in place writes nothing")

	l.nodes = ""
	ok, out = p.gate(t, l)
	require.True(t, ok, out)
	require.True(t, registeredIn(l, p), l.nodes)
	require.Equal(t, claims, l.claimWrites)
	require.Contains(t, out, "recording it again")

	require.NoError(t, os.WriteFile(p.nodeIDFile, []byte("nNewNodeCNTRL\n"), 0o644))
	ok, out = p.gate(t, l)
	require.True(t, ok, out)
	flag, err := os.ReadFile(p.flag)
	require.NoError(t, err)
	require.Contains(t, l.nodes, "/"+p.name+"/nNewNodeCNTRL")
	require.Contains(t, l.nodes, "/"+p.name+"/"+gateNodeID(p.name))
	require.True(t, strings.HasPrefix(string(flag), "nNewNodeCNTRL "), "the flag names the new node first: %q", flag)
	require.Equal(t, claims, l.claimWrites)
}

// A node id that is not alphanumeric, as no StableNodeID is, is never written into the gate's
// merge patch: the pass asks nothing, writes nothing and sets no flag. The negative control is
// every other gate test, whose ids are alphanumeric.
func TestDispatchCertificateBudgetRefusesAnInjectedNodeID(t *testing.T) {
	l := newFrontDoorLedger(t, gateClaimsAt(time.Hour))
	p := newGatePod(t, frontDoorTestPod+"a")
	injected := `x","x":"y"}},"data":{"tls.key":"QUFBQQ=="},"z":{"a":{"b":"`
	require.NoError(t, os.WriteFile(p.nodeIDFile, []byte(injected+"\n"), 0o644))
	ok, out := p.gate(t, l)
	require.False(t, ok, out)
	require.Zero(t, l.requests)
	require.Zero(t, l.writes)
	require.Contains(t, out, "not alphanumeric")
}

// The gate writes the two annotations it keeps and no other, under Dispatch's own keys.
func TestDispatchCertificateBudgetWritesOnlyItsTwoAnnotations(t *testing.T) {
	l := newFrontDoorLedger(t, "")
	p := newGatePod(t, frontDoorTestPod+"a")
	ok, out := p.gate(t, l)
	require.True(t, ok, out)
	require.Len(t, l.annotationsReceived, 1)
	var keys []string
	for key := range l.annotationsReceived[0] {
		keys = append(keys, key)
	}
	require.ElementsMatch(t, []string{"legion.dev/certificate-claims", "legion.dev/front-door-nodes"}, keys)
}

// An epoch the gate did not write itself, with a leading zero or more digits than the shell's
// integers hold, is skipped as malformed, like a non-numeric one, in both annotations: the pass
// still decides, here taking the third claim beside the two live ones. Shell arithmetic reads a
// leading zero as octal and fails on 8 or 9, and dash fails on an overflow; either error ends the
// gate's background subshell, which nothing restarts, so no pod reading that ledger would ever
// advertise again. The negative control is TestDispatchCertificateBudgetClaims, whose epochs are
// well formed.
func TestDispatchCertificateBudgetSkipsAMalformedEpoch(t *testing.T) {
	malformed := fmt.Sprintf("08/%sother8 0177/%sother9 99999999999999999999999/%sother10", frontDoorTestPod, frontDoorTestPod, frontDoorTestPod)
	l := newFrontDoorLedger(t, malformed+" "+gateClaimsAt(time.Hour, 2*time.Hour))
	l.nodes = fmt.Sprintf("09/%sgone/ngoneCNTRL", frontDoorTestPod)
	p := newGatePod(t, frontDoorTestPod+"a")
	ok, out := p.gate(t, l)
	require.True(t, ok, out)
	require.NotContains(t, out, "Illegal number")
	require.NotContains(t, out, "arithmetic")
	require.Equal(t, 1, l.claimWrites, out)
	require.Contains(t, l.claims, "/"+p.name)
	require.True(t, registeredIn(l, p), l.nodes)
}
