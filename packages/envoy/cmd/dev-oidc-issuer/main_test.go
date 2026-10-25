package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/oidc"
)

const (
	testAudience = "legion-broker-dev"
	testAccount  = "system:serviceaccount:example-ns:example-worker"
)

// lockedBuffer is the command's stderr, read by the test while the command still writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var listening = regexp.MustCompile(`issuer listening url=(\S+)`)

// TestTheTokenFileVerifiesAgainstTheIssuerAndIsRewrittenBeforeItLapses runs the issuer with a
// lifetime of seconds: the file it writes before it reports its URL holds a token the broker's own
// verifier accepts for the account and audience asked for, bound to a pod and living exactly the
// lifetime, and four fifths of the way through that life the file holds a new token for the same
// pod, as a kubelet rewrites a projected token.
func TestTheTokenFileVerifiesAgainstTheIssuerAndIsRewrittenBeforeItLapses(t *testing.T) {
	// Long enough that the rewrite at four fifths of the life has real slack on a loaded
	// machine: a shorter one let the first token lapse between the rewriter waking and the
	// test's next read, which the read below would have called a token that does not verify.
	const lifetime = 6 * time.Second
	file := filepath.Join(t.TempDir(), "token")
	var stderr lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- serve(ctx, []tokenSpec{{audience: testAudience, account: testAccount, file: file}}, lifetime, &stderr)
	}()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("serve returned %d after its context ended, want 0\n%s", code, stderr.String())
		}
	})

	var issuerURL string
	for deadline := time.Now().Add(10 * time.Second); issuerURL == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if match := listening.FindStringSubmatch(stderr.String()); match != nil {
			issuerURL = match[1]
		}
	}
	if issuerURL == "" {
		t.Fatalf("the issuer never logged its URL:\n%s", stderr.String())
	}
	verifier, err := oidc.New(context.Background(), issuerURL, testAudience)
	if err != nil {
		t.Fatalf("oidc.New(%s): %v", issuerURL, err)
	}
	readRaw := func() string {
		t.Helper()
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read the token file the issuer reported written: %v", err)
		}
		return strings.TrimSpace(string(raw))
	}
	verify := func(token string) enroll.AccountClaims {
		t.Helper()
		claims, err := enroll.K8sPodVerifier{Verifier: verifier}.VerifyAccount(context.Background(), token)
		if err != nil {
			t.Fatalf("the token in %s does not verify against the issuer it names: %v", file, err)
		}
		return claims
	}

	first := readRaw()
	claims := verify(first)
	if claims.Subject != testAccount || claims.PodUID == "" || claims.Expiry.Sub(claims.IssuedAt) != lifetime {
		t.Fatalf("the token names subject %q, pod %q and a life of %s, want %s, a pod, and %s",
			claims.Subject, claims.PodUID, claims.Expiry.Sub(claims.IssuedAt), testAccount, lifetime)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the token file's mode is %v, want 0600: it is a credential", info.Mode().Perm())
	}

	for deadline := time.Now().Add(3 * lifetime); ; time.Sleep(20 * time.Millisecond) {
		// Only the rewritten token is verified: the first one is allowed to lapse while this
		// loop waits, and verifying it again would make a slow machine look like a broken
		// issuer.
		second := readRaw()
		if second != first {
			rewritten := verify(second)
			if rewritten.PodUID != claims.PodUID || rewritten.Subject != testAccount {
				t.Fatalf("the rewritten token names %s's pod %q, want %s's pod %q kept across rewrites", rewritten.Subject, rewritten.PodUID, testAccount, claims.PodUID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the token file still holds its first token %s after it was minted, want it rewritten before it lapses", 3*lifetime)
		}
	}
}

// TestRunRefusesATokenItCannotMint pins the usage refusals: a --token that names no file, and one
// whose subject is not a ServiceAccount, each exit 2 naming what is wrong, before any issuer runs.
func TestRunRefusesATokenItCannotMint(t *testing.T) {
	for _, tc := range []struct{ value, says string }{
		{"legion-broker-dev=" + testAccount, "want <audience>=<service account>=<file>"},
		{"legion-broker-dev=ada@example.com=/tmp/token", `service account "ada@example.com" is not system:serviceaccount:<namespace>:<name>`},
	} {
		var stderr lockedBuffer
		if code := run(context.Background(), []string{"--token", tc.value}, &stderr); code != exitUsageError || !strings.Contains(stderr.String(), tc.says) {
			t.Errorf("--token %s: exit %d, stderr %q; want %d saying %q", tc.value, code, stderr.String(), exitUsageError, tc.says)
		}
	}
}
