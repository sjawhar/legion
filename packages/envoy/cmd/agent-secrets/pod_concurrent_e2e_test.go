//go:build unix

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
// The broker's per-pod launcher credential arrived with cybermonklabs/legion#2777, staged over
// two releases: that change's migration adds the per-pod indexes and keeps 0015's
// `launcher_credentials_live_subject`, so a roll of the broker serves the previous generation
// beside it, and one ServiceAccount still holds one live credential until the follow-up
// migration of the release after it drops that index (dispatch://INFRA-1567). So this drives
// the schema that release leaves, through the helper #2777 added for exactly that
// (storetest.PerPodLoginSchema), as #2777's own per-pod rows do — rather than skipping until
// the drop lands, which would leave two pods of one account unproven for a release.
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
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// TestTwoPodsOfOneAccountEnrolThemselvesAtOnce: two pods running as one ServiceAccount, each with
// its own pod UID, its own projected token and its own key directory, run `enroll --pod`
// concurrently against one broker. Both must succeed and keep working: each ends up with a
// launcher credential of its own, and the enrollment each made is live under its own key.
func TestTwoPodsOfOneAccountEnrolThemselvesAtOnce(t *testing.T) {
	rig := brokertest.NewRig(t)
	// The schema of the release after #2777's, which is where each pod of a ServiceAccount holds
	// a launcher credential of its own. Until then one pod's login takes the other's key over
	// and the first pod is 401 LAUNCHER_INVALID, which is the dependency this test states rather
	// than a fault of this command.
	storetest.PerPodLoginSchema(t, rig.Store)
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

// TestAPodEnrolsItselfAgainstTheRealBroker is this mode against real handlers and real Postgres,
// one pod, which needs no schema the tree does not have, so it runs everywhere the two-pod row
// above skips. The stub broker verifies nothing, so until this the shape of what `enroll --pod`
// sends — the signed request object, the service it names, the pod token, the launcher proof
// over the enrollment — was pinned only against a fake that would have taken anything. Here the
// broker refuses each of them if it is wrong.
//
// A re-run on the same directory is driven too, because the broker answers a key whose
// enrollment is live with that enrollment and no fresh lease: the run has to renew it, and the
// renewal is a different proof (the session key's, not the login key's) that the real verifier
// checks.
func TestAPodEnrolsItselfAgainstTheRealBroker(t *testing.T) {
	rig := brokertest.NewRig(t)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	podUID := uuid.NewString()
	tokenFile := writePodToken(t, rig.MintAccountPodToken(t, brokertest.DaemonAccount, podUID, time.Hour))
	out := filepath.Join(t.TempDir(), "keys")

	var stdout, stderr strings.Builder
	if code := cmdEnrollPod("legion-daemon", tokenFile, out, &stdout, &stderr); code != 0 {
		t.Fatalf("enroll --pod against the real broker = exit %d, stderr %q", code, stderr.String())
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		t.Fatalf("enroll --pod printed no enrollment id; stderr %q", stderr.String())
	}
	if written, err := readTrimmed(filepath.Join(out, enrollmentFile)); err != nil || written != id {
		t.Fatalf("the enrollment file = %q (%v), want the id it printed, %s", written, err, id)
	}

	// The broker's own row: the enrollment is this pod's, live, and under a launcher credential
	// the account's own token login made.
	var kind, runtimeID, subject string
	if err := rig.Store.Pool.QueryRow(context.Background(),
		`select e.kind, e.runtime_id, coalesce(c.subject,'') from enrollments e
			join launcher_credentials c on c.id = e.launcher_credential_id
			where e.id=$1 and e.revoked_at is null and e.lease_expires_at > now()`, id).
		Scan(&kind, &runtimeID, &subject); err != nil {
		t.Fatalf("read the enrollment the broker made: %v", err)
	}
	if kind != "pod" || runtimeID != podUID || subject != brokertest.DaemonAccount {
		t.Fatalf("the enrollment = kind %s, runtime %s, under %s; want a pod enrollment of %s under %s", kind, runtimeID, subject, podUID, brokertest.DaemonAccount)
	}

	// The re-run: same key, same enrollment, and a lease the renewal pushed out.
	var before time.Time
	if err := rig.Store.Pool.QueryRow(context.Background(), `select lease_expires_at from enrollments where id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.Store.Pool.Exec(context.Background(),
		`update enrollments set lease_expires_at = now() + interval '1 minute' where id=$1`, id); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdEnrollPod("legion-daemon", tokenFile, out, &stdout, &stderr); code != 0 {
		t.Fatalf("the re-run = exit %d, stderr %q", code, stderr.String())
	}
	if again := strings.TrimSpace(stdout.String()); again != id {
		t.Fatalf("the re-run's enrollment = %s, want the one the key already holds, %s", again, id)
	}
	var after time.Time
	if err := rig.Store.Pool.QueryRow(context.Background(), `select lease_expires_at from enrollments where id=$1`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.After(before.Add(-time.Second)) {
		t.Fatalf("the lease after the re-run = %s, want it renewed past the minute it was cut to (%s)", after, before)
	}
}
