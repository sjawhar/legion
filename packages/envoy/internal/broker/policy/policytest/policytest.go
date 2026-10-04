// Package policytest builds a live policy for tests: secrets held by a secrets.Local, under one
// namespace and encrypted with one agent-secrets key, loaded by the real policy.Loader.
package policytest

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

const (
	// Prefix is the namespace every Secret sits under.
	Prefix = "example/agent-secrets/"
	// KeyARN is the agent-secrets key every Secret is encrypted with (an AWS documentation
	// account).
	KeyARN = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// Secret is the namespace secret a session asks for as name (an environment-variable name such as
// DEEL_API_KEY), tagged with owner and tier, encrypted with KeyARN, holding value.
func Secret(name, owner, tier, value string) secrets.LocalSecret {
	return secrets.LocalSecret{
		Name:     ID(name),
		KmsKeyID: KeyARN,
		Tags:     map[string]string{policy.TagOwner: owner, policy.TagTier: tier},
		Value:    value,
	}
}

// ID is the Secrets Manager name of the secret a session asks for as name.
func ID(name string) string {
	return Prefix + strings.ReplaceAll(strings.ToLower(name), "_", "-")
}

// Loader is the policy.Loader over store, with services registered.
func Loader(store *secrets.Local, services ...string) policy.Loader {
	return policy.Loader{Secrets: store, Aliases: store, Prefix: Prefix, KeyARN: KeyARN, Services: services}
}

// Current loads store's policy, as the broker does at boot; Refresh rereads it after store
// changes. It stops reloading when t ends.
func Current(t testing.TB, store *secrets.Local, services ...string) *policy.Current {
	t.Helper()
	cur, err := policy.NewCurrent(t.Context(), Loader(store, services...), time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	return cur
}

// CaptureLog points the default slog handler, which the broker logs through, at a buffer with no
// timestamp until t ends, so a test reads the exact lines the broker writes; the reload ticker's
// goroutine may write while the test reads.
func CaptureLog(t testing.TB) fmt.Stringer {
	t.Helper()
	logged := &lockedBuffer{}
	flags, output := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetFlags(flags); log.SetOutput(output) })
	return logged
}

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
