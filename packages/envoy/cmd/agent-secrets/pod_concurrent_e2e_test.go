// packages/envoy/cmd/agent-secrets/pod_concurrent_e2e_test.go
//
// Two pods of one ServiceAccount enrolling themselves at the same time, against the real broker
// (brokertest.NewRig: real handlers, real Postgres). It skips without BROKER_TEST_DATABASE_URL,
// as every rig test does.
//
// `enroll --pod` makes a launcher key of its own on every run, and the enrol.lock it takes is one
// directory's: two pods have two directories and never meet on it, which is right — no lock
// between separate pods could be taken anyway. So the pods are safe together only if the broker
// gives each pod a launcher credential of its own. A broker that keeps one live credential per
// ServiceAccount cannot: the second pod's login moves that one credential onto its own key, and
// the first pod's enrollment, signed with the key the credential no longer holds, is refused
// `401 LAUNCHER_INVALID`.
//
// The broker's per-pod launcher credential is cybermonklabs/legion#2777, staged over two
// releases: that change's migration adds the per-pod indexes and keeps 0015's
// `launcher_credentials_live_subject`, so a roll of the broker serves the previous generation
// beside it, and one ServiceAccount still holds one live credential until the follow-up
// migration of the release after it drops that index. So the thing to look for is that index
// being gone, which is what the broker's own `enroll.perPodLogins` reads — looking for the
// per-pod index instead would start this test one release early, while the schema still refuses
// a second pod's row.
//
// This test is that dependency, stated in the one place a reader of either change will run: on a
// tree whose broker is still one credential per account it skips, naming #2777, and it runs the
// moment the schema it needs is in the tree.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
)

// perPodLauncherCredentials reports whether rig's broker gives each pod of a ServiceAccount a
// launcher credential of its own, which it does once 0015's one-live-credential-per-subject
// index is gone: the same read the broker's own enroll.perPodLogins makes.
func perPodLauncherCredentials(t *testing.T, rig *brokertest.Rig) bool {
	t.Helper()
	var legacy bool
	if err := rig.Store.Pool.QueryRow(context.Background(),
		`select to_regclass('launcher_credentials_live_subject') is not null`).Scan(&legacy); err != nil {
		t.Fatalf("read the broker's launcher-credential indexes: %v", err)
	}
	return !legacy
}

// TestTwoPodsOfOneAccountEnrolThemselvesAtOnce: two pods running as one ServiceAccount, each with
// its own pod UID, its own projected token and its own key directory, run `enroll --pod`
// concurrently against one broker. Both must succeed and keep working: each ends up with a
// launcher credential of its own, and the enrollment each made is live under its own key.
func TestTwoPodsOfOneAccountEnrolThemselvesAtOnce(t *testing.T) {
	rig := brokertest.NewRig(t)
	if !perPodLauncherCredentials(t, rig) {
		t.Skip("this broker keeps one live launcher credential per ServiceAccount (launcher_credentials_live_subject is still there), so a second pod of an account takes the first pod's key over; it needs the per-pod login of cybermonklabs/legion#2777, which stages that index's drop into the release after it, and runs from there")
	}
	t.Setenv("AGENT_SECRETS_URL", rig.URL)

	type pod struct {
		uid    string
		out    string
		code   int
		stdout strings.Builder
		stderr strings.Builder
	}
	pods := []*pod{
		{uid: uuid.NewString(), out: filepath.Join(t.TempDir(), "pod-a")},
		{uid: uuid.NewString(), out: filepath.Join(t.TempDir(), "pod-b")},
	}
	// Every token is minted before the goroutines start: minting fails t, which only this
	// goroutine may do.
	tokenFiles := make([]string, len(pods))
	for i, p := range pods {
		tokenFiles[i] = writePodToken(t, rig.MintAccountPodToken(t, brokertest.DaemonAccount, p.uid, time.Hour))
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	for i, p := range pods {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			p.code = cmdEnrollPod("legion-daemon", tokenFiles[i], p.out, &p.stdout, &p.stderr)
		}()
	}
	close(start)
	wait.Wait()

	for _, p := range pods {
		if p.code != 0 {
			t.Errorf("pod %s: enroll --pod exit %d, stderr %q; want both pods enrolled", p.uid, p.code, p.stderr.String())
			continue
		}
		if got := strings.TrimSpace(p.stdout.String()); got == "" {
			t.Errorf("pod %s printed no enrollment id", p.uid)
		}
	}
	if t.Failed() {
		return
	}

	// Each pod holds a live credential of its own, bound to its own pod, and the enrollment each
	// made is that pod's: a broker that had moved one credential between them would leave one
	// pod's row gone and its enrollment under the other's credential.
	var credentials int
	if err := rig.Store.Pool.QueryRow(context.Background(),
		`select count(*) from launcher_credentials where subject=$1 and revoked_at is null`, brokertest.DaemonAccount).
		Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if credentials != len(pods) {
		t.Fatalf("live launcher credentials of %s = %d, want one per pod (%d)", brokertest.DaemonAccount, credentials, len(pods))
	}
	for _, p := range pods {
		id, err := readTrimmed(filepath.Join(p.out, enrollmentFile))
		if err != nil {
			t.Fatalf("pod %s: read its enrollment file: %v", p.uid, err)
		}
		var runtimeID string
		if err := rig.Store.Pool.QueryRow(context.Background(),
			`select runtime_id from enrollments where id=$1 and revoked_at is null and lease_expires_at > now()`, id).
			Scan(&runtimeID); err != nil {
			t.Fatalf("pod %s: read its enrollment %s: %v", p.uid, id, err)
		}
		if runtimeID != p.uid {
			t.Fatalf("enrollment %s is runtime %s, want the pod that made it, %s", id, runtimeID, p.uid)
		}
		if _, err := os.Stat(filepath.Join(p.out, keyFile)); err != nil {
			t.Fatalf("pod %s: its session key: %v", p.uid, err)
		}
	}
}
