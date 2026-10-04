package enroll

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// insertRecord writes a credential-request record of kind naming approver, and returns its id.
func insertRecord(t *testing.T, svc *Service, kind, approver string) string {
	t.Helper()
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := svc.Store.Pool.Exec(context.Background(), `insert into credential_requests (id, body, kind, approver, expires_at)
		values ($1,'body',$2,$3, now() + interval '1 hour')`, id, kind, approver); err != nil {
		t.Fatalf("insert %s record: %v", kind, err)
	}
	return id
}

// mintApprovedCredential mints a launcher credential as machine.Service.ApplyDecision does once
// approver approves a machine login: from a launcher_credential record naming approver, with
// approver its operator for a person's own machine, or no operator for a service's login.
func mintApprovedCredential(t *testing.T, svc *Service, approver string, service *string, host string) Credential {
	t.Helper()
	var operator *string
	if service == nil {
		operator = &approver
	}
	return mintCredentialFrom(t, svc, operator, service, host, insertRecord(t, svc, "launcher_credential", approver))
}

// insertLiveGrant writes a granted request and its live grant under enrollmentID directly (grant
// issuance is requests.Machine's), and returns the grant's id.
func insertLiveGrant(t *testing.T, svc *Service, enrollmentID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	requestID, grantID := uuid.New(), uuid.New()
	if _, err := svc.Store.Pool.Exec(ctx, `insert into requests (id, enrollment_id, reason, state, rules_version, lifetime_seconds)
		values ($1,$2,'test fixture','granted','v1',3600)`, requestID, enrollmentID); err != nil {
		t.Fatalf("insert request fixture: %v", err)
	}
	if _, err := svc.Store.Pool.Exec(ctx, `insert into grants (id, request_id, enrollment_id, expires_at) values ($1,$2,$3, now() + interval '1 hour')`,
		grantID, requestID, enrollmentID); err != nil {
		t.Fatalf("insert grant fixture: %v", err)
	}
	return grantID
}

// liveCredentialIDs is LiveCredentials(operator) reduced to its ids, in its order.
func liveCredentialIDs(t *testing.T, svc *Service, operator string) []uuid.UUID {
	t.Helper()
	creds, err := svc.LiveCredentials(context.Background(), operator)
	if err != nil {
		t.Fatalf("LiveCredentials(%s): %v", operator, err)
	}
	ids := make([]uuid.UUID, len(creds))
	for i, c := range creds {
		ids[i] = c.ID
	}
	return ids
}

// TestRevokingAMachineLoginEndsEverySessionItEnrolled: a person revoking their devbox's machine
// login ends that credential and every session it enrolled — each session's grant revoked and
// pending request cancelled in their name — while their other machine's session goes on; the
// credential leaves their list and enrolls nothing more, and revoking it again changes nothing.
func TestRevokingAMachineLoginEndsEverySessionItEnrolled(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	devbox := mintApprovedCredential(t, svc, "ada@example.com", nil, "devbox")
	laptop := mintApprovedCredential(t, svc, "ada@example.com", nil, "laptop")
	host, err := svc.Create(ctx, devbox, Enrollment{Kind: "host", RuntimeID: "host-1", Thumbprint: "tp-host-1"})
	if err != nil {
		t.Fatalf("Create(host): %v", err)
	}
	box, err := svc.Create(ctx, devbox, Enrollment{Kind: "box", RuntimeID: "box-1", Thumbprint: "tp-box-1"})
	if err != nil {
		t.Fatalf("Create(box): %v", err)
	}
	other, err := svc.Create(ctx, laptop, Enrollment{Kind: "host", RuntimeID: "host-2", Thumbprint: "tp-host-2"})
	if err != nil {
		t.Fatalf("Create(the laptop's host): %v", err)
	}
	grant := insertLiveGrant(t, svc, host.ID)
	pending := insertPendingRequest(t, svc, box.ID)
	if got := liveCredentialIDs(t, svc, "ada@example.com"); !slices.Equal(got, []uuid.UUID{laptop.ID, devbox.ID}) {
		t.Fatalf("LiveCredentials before the revoke = %v, want the laptop then the devbox", got)
	}

	if err := svc.RevokeCredential(ctx, devbox.ID.String(), " Ada@Example.com "); err != nil {
		t.Fatalf("RevokeCredential by the person who approved it: %v", err)
	}

	for _, e := range []Enrollment{host, box} {
		if _, live, err := svc.Lookup(ctx, e.ID.String()); err != nil || live {
			t.Fatalf("Lookup(%s session) after the revoke = live %v, %v; want ended", e.Kind, live, err)
		}
	}
	if _, live, err := svc.Lookup(ctx, other.ID.String()); err != nil || !live {
		t.Fatalf("Lookup(the laptop's session) = live %v, %v; want it still live", live, err)
	}
	var revokedBy *string
	if err := svc.Store.Pool.QueryRow(ctx, `select revoked_by from grants where id=$1`, grant).Scan(&revokedBy); err != nil || revokedBy == nil || *revokedBy != "human:ada@example.com" {
		t.Fatalf("grant revoked_by = %v (%v), want human:ada@example.com", revokedBy, err)
	}
	if state, by, audits, event := requestState(t, svc, pending); state != "cancelled" || by != "human:ada@example.com" || audits != 1 || event != "cancelled by human:ada@example.com" {
		t.Fatalf("pending request after the revoke: state=%s decided_by=%s audit rows=%d record event=%q, want cancelled by human:ada@example.com", state, by, audits, event)
	}
	if got := liveCredentialIDs(t, svc, "ada@example.com"); !slices.Equal(got, []uuid.UUID{laptop.ID}) {
		t.Fatalf("LiveCredentials after the revoke = %v, want the laptop alone", got)
	}
	if _, err := svc.Create(ctx, devbox, Enrollment{Kind: "host", RuntimeID: "host-3", Thumbprint: "tp-host-3"}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Create under the revoked credential = %v, want ErrUnauthenticated", err)
	}

	if err := svc.RevokeCredential(ctx, devbox.ID.String(), "ada@example.com"); err != nil {
		t.Fatalf("RevokeCredential again: %v", err)
	}
	var credentialAudits, enrollmentAudits int
	if err := svc.Store.Pool.QueryRow(ctx, `select (select count(*) from audit where kind='launcher_credential.revoked' and detail->>'credential_id'=$1),
		(select count(*) from audit where kind='enrollment.revoked' and actor='human:ada@example.com')`, devbox.ID.String()).Scan(&credentialAudits, &enrollmentAudits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if credentialAudits != 1 || enrollmentAudits != 2 {
		t.Fatalf("audit rows: %d launcher_credential.revoked, %d enrollment.revoked; want 1 and 2", credentialAudits, enrollmentAudits)
	}
}

// TestOnlyTheApproverRevokesAMachineLogin: another person, an empty name, and another person
// revoking a service's login someone else approved are each ErrNotApprover, an unknown id is
// ErrNoCredential, and none of them ends anything. A person's list holds the credentials they
// approved that can still reach a secret, a service's login among them, never another person's:
// an expired login is listed, marked expired, while a session it enrolled still runs (a session
// renews with its own key, past its login's expiry), and an expired one with no session is not.
// Revoking the expired login ends that session and takes it off the list.
func TestOnlyTheApproverRevokesAMachineLogin(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	ada := mintApprovedCredential(t, svc, "ada@example.com", nil, "devbox")
	bob := mintApprovedCredential(t, svc, "bob@example.com", nil, "bobs-box")
	service := mintApprovedCredential(t, svc, "ada@example.com", str("legion-daemon"), "cluster")
	expired := mintApprovedCredential(t, svc, "ada@example.com", nil, "old-box")
	stale := mintApprovedCredential(t, svc, "ada@example.com", nil, "older-box")
	staleBox, err := svc.Create(ctx, stale, Enrollment{Kind: "box", RuntimeID: "box-stale", Thumbprint: "tp-stale"})
	if err != nil {
		t.Fatalf("Create(box under the login that then expires): %v", err)
	}
	if _, err := svc.Store.Pool.Exec(ctx, `update launcher_credentials set expires_at = now() - interval '1 minute' where id = any($1)`, []uuid.UUID{expired.ID, stale.ID}); err != nil {
		t.Fatalf("expire two credentials: %v", err)
	}
	if _, err := svc.Renew(ctx, staleBox.ID.String()); err != nil {
		t.Fatalf("Renew(the box) after its login expired = %v; a session outlives its login's expiry", err)
	}
	enr, err := svc.Create(ctx, ada, Enrollment{Kind: "host", RuntimeID: "host-ada", Thumbprint: "tp-ada"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	listed, err := svc.LiveCredentials(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("LiveCredentials(ada): %v", err)
	}
	type row struct {
		ID      uuid.UUID
		Expired bool
	}
	got := make([]row, len(listed))
	for i, c := range listed {
		got[i] = row{c.ID, c.Expired}
	}
	if want := []row{{stale.ID, true}, {service.ID, false}, {ada.ID, false}}; !slices.Equal(got, want) {
		t.Fatalf("LiveCredentials(ada) = %v, want the expired login whose box still runs (expired), the service login she approved, then her live devbox: %v", got, want)
	}
	if got := liveCredentialIDs(t, svc, "bob@example.com"); !slices.Equal(got, []uuid.UUID{bob.ID}) {
		t.Fatalf("LiveCredentials(bob) = %v, want his own alone", got)
	}

	for _, tc := range []struct {
		name, id, approver string
		want               error
	}{
		{"another person", ada.ID.String(), "bob@example.com", ErrNotApprover},
		{"no one", ada.ID.String(), "  ", ErrNotApprover},
		{"another person, a service's login", service.ID.String(), "bob@example.com", ErrNotApprover},
		{"another person, an expired login", stale.ID.String(), "bob@example.com", ErrNotApprover},
		{"an unknown id", uuid.NewString(), "ada@example.com", ErrNoCredential},
	} {
		if err := svc.RevokeCredential(ctx, tc.id, tc.approver); !errors.Is(err, tc.want) {
			t.Fatalf("RevokeCredential(%s) = %v, want %v", tc.name, err, tc.want)
		}
	}
	for name, e := range map[string]Enrollment{"ada's session": enr, "the expired login's box": staleBox} {
		if _, live, err := svc.Lookup(ctx, e.ID.String()); err != nil || !live {
			t.Fatalf("Lookup(%s) after the refused revokes = live %v, %v; want live", name, live, err)
		}
	}
	var revoked int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from launcher_credentials where revoked_at is not null`).Scan(&revoked); err != nil || revoked != 0 {
		t.Fatalf("revoked credentials after the refused revokes = %d (%v), want none", revoked, err)
	}

	if err := svc.RevokeCredential(ctx, stale.ID.String(), "ada@example.com"); err != nil {
		t.Fatalf("RevokeCredential(the expired login) by ada: %v", err)
	}
	if _, live, err := svc.Lookup(ctx, staleBox.ID.String()); err != nil || live {
		t.Fatalf("Lookup(the expired login's box) after its revoke = live %v, %v; want ended", live, err)
	}
	if got := liveCredentialIDs(t, svc, "ada@example.com"); !slices.Equal(got, []uuid.UUID{service.ID, ada.ID}) {
		t.Fatalf("LiveCredentials(ada) after revoking the expired login = %v, want the service login then her devbox", got)
	}
}

// TestAnApproverRevokesAServiceLoginAndEveryPodItEnrolled: a service's login, which has no
// operator, is listed for the person who approved it and revoked by them alone, and revoking it
// ends every pod it enrolled — each pod's grants revoked and pending requests cancelled in the
// approver's name, its lease no longer renewable — while a pod another service login enrolled goes
// on. The audit row names the service and both pods.
func TestAnApproverRevokesAServiceLoginAndEveryPodItEnrolled(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	issuer, key := withPodVerifier(t, svc)
	daemon := mintApprovedCredential(t, svc, "ada@example.com", str("legion-daemon"), "cluster")
	other := mintApprovedCredential(t, svc, "bob@example.com", str("legion-daemon"), "other-cluster")
	pod := func(cred Credential, uid, slot string) Enrollment {
		t.Helper()
		enr, err := svc.Create(ctx, cred, Enrollment{
			Kind: "pod", RuntimeID: uid, Slot: slot, Thumbprint: "tp-" + uid + "-" + slot,
			PodToken: mintPodToken(t, issuer, key, "system:serviceaccount:legion:worker", uid),
		})
		if err != nil {
			t.Fatalf("Create(pod %s, slot %s): %v", uid, slot, err)
		}
		return enr
	}
	implementer := pod(daemon, "pod-1", "implementer-g1")
	reviewer := pod(daemon, "pod-1", "reviewer-g1")
	elsewhere := pod(other, "pod-2", "implementer-g1")
	grant := insertLiveGrant(t, svc, implementer.ID)
	pending := insertPendingRequest(t, svc, reviewer.ID)
	creds, err := svc.LiveCredentials(ctx, "ada@example.com")
	if err != nil || len(creds) != 1 || creds[0].ID != daemon.ID || creds[0].Host != "cluster" || creds[0].Service == nil || *creds[0].Service != "legion-daemon" {
		t.Fatalf("LiveCredentials(ada) = %+v, %v; want the legion-daemon login on cluster she approved", creds, err)
	}

	if err := svc.RevokeCredential(ctx, daemon.ID.String(), "bob@example.com"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("RevokeCredential by bob, who approved another login = %v, want ErrNotApprover", err)
	}
	if _, live, err := svc.Lookup(ctx, implementer.ID.String()); err != nil || !live {
		t.Fatalf("Lookup(implementer pod) after bob's refused revoke = live %v, %v; want live", live, err)
	}
	if err := svc.RevokeCredential(ctx, daemon.ID.String(), "ada@example.com"); err != nil {
		t.Fatalf("RevokeCredential by ada, who approved it: %v", err)
	}

	for _, e := range []Enrollment{implementer, reviewer} {
		if _, live, err := svc.Lookup(ctx, e.ID.String()); err != nil || live {
			t.Fatalf("Lookup(pod slot %s) after the revoke = live %v, %v; want ended", e.Slot, live, err)
		}
		if _, err := svc.Renew(ctx, e.ID.String()); !errors.Is(err, ErrNotLive) {
			t.Fatalf("Renew(pod slot %s) after the revoke = %v, want ErrNotLive", e.Slot, err)
		}
	}
	if _, live, err := svc.Lookup(ctx, elsewhere.ID.String()); err != nil || !live {
		t.Fatalf("Lookup(the other login's pod) = live %v, %v; want it still live", live, err)
	}
	var revokedBy *string
	if err := svc.Store.Pool.QueryRow(ctx, `select revoked_by from grants where id=$1`, grant).Scan(&revokedBy); err != nil || revokedBy == nil || *revokedBy != "human:ada@example.com" {
		t.Fatalf("grant revoked_by = %v (%v), want human:ada@example.com", revokedBy, err)
	}
	if state, by, _, event := requestState(t, svc, pending); state != "cancelled" || by != "human:ada@example.com" || event != "cancelled by human:ada@example.com" {
		t.Fatalf("pending request after the revoke: state=%s decided_by=%s record event=%q, want cancelled by human:ada@example.com", state, by, event)
	}
	var service string
	var ended int
	if err := svc.Store.Pool.QueryRow(ctx, `select detail->>'service', jsonb_array_length(detail->'enrollments') from audit
		where kind='launcher_credential.revoked' and actor='human:ada@example.com' and detail->>'credential_id'=$1`, daemon.ID.String()).Scan(&service, &ended); err != nil {
		t.Fatalf("read the launcher_credential.revoked audit row: %v", err)
	}
	if service != "legion-daemon" || ended != 2 {
		t.Fatalf("audit row names service %q and %d enrollments, want legion-daemon and 2", service, ended)
	}
	if got := liveCredentialIDs(t, svc, "ada@example.com"); len(got) != 0 {
		t.Fatalf("LiveCredentials(ada) after the revoke = %v, want none", got)
	}
	if got := liveCredentialIDs(t, svc, "bob@example.com"); !slices.Equal(got, []uuid.UUID{other.ID}) {
		t.Fatalf("LiveCredentials(bob) = %v, want the login he approved, untouched", got)
	}
}

// TestOnlyALaunchersOwnRecordNamesWhoMayListAndRevokeIt: who may list and revoke a credential is
// the approver of the launcher_credential record it was minted from, and nothing else — not its
// operator column (a credential whose operator is ada but whose record bob approved, a shape the
// broker never mints), not a record of another kind that names ada, and not a credential with no
// record at all. None of the three is ada's to list or revoke; the first is bob's, and the other
// two, minted from no launcher_credential record, are no machine login anyone may revoke
// (ErrNoCredential).
func TestOnlyALaunchersOwnRecordNamesWhoMayListAndRevokeIt(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	bobs := mintCredentialFrom(t, svc, str("ada@example.com"), nil, "bobs-record", insertRecord(t, svc, "launcher_credential", "bob@example.com"))
	secret := mintCredentialFrom(t, svc, str("ada@example.com"), nil, "secret-record", insertRecord(t, svc, "agent_secret", "ada@example.com"))
	unbacked := mintCredential(t, svc, str("ada@example.com"), nil, "no-record")

	if got := liveCredentialIDs(t, svc, "ada@example.com"); len(got) != 0 {
		t.Fatalf("LiveCredentials(ada) = %v, want none: she approved no launcher record", got)
	}
	if got := liveCredentialIDs(t, svc, "bob@example.com"); !slices.Equal(got, []uuid.UUID{bobs.ID}) {
		t.Fatalf("LiveCredentials(bob) = %v, want the one minted from the record he approved", got)
	}
	for _, tc := range []struct {
		name string
		c    Credential
		want error
	}{
		{"another approver's record", bobs, ErrNotApprover},
		{"a record of another kind", secret, ErrNoCredential},
		{"no record", unbacked, ErrNoCredential},
	} {
		if err := svc.RevokeCredential(ctx, tc.c.ID.String(), "ada@example.com"); !errors.Is(err, tc.want) {
			t.Fatalf("RevokeCredential(%s) by ada = %v, want %v", tc.name, err, tc.want)
		}
	}
	var revoked int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from launcher_credentials where revoked_at is not null`).Scan(&revoked); err != nil || revoked != 0 {
		t.Fatalf("revoked credentials after ada's refused revokes = %d (%v), want none", revoked, err)
	}
	if err := svc.RevokeCredential(ctx, bobs.ID.String(), "bob@example.com"); err != nil {
		t.Fatalf("RevokeCredential by bob, its record's approver: %v", err)
	}
}

// awaitLockWaiters waits until n backends of svc's server wait on a lock behind tx's backend:
// blocked by it, or queued behind a backend that is.
func awaitLockWaiters(t *testing.T, svc *Service, tx pgx.Tx, n int) {
	t.Helper()
	ctx := context.Background()
	var holder int
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatalf("read the holder's backend: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := svc.Store.Pool.QueryRow(ctx, `with recursive waiters(pid) as (
				select pid from pg_stat_activity where $1 = any(pg_blocking_pids(pid))
				union select a.pid from pg_stat_activity a join waiters w on w.pid = any(pg_blocking_pids(a.pid)))
			select count(*) from waiters join pg_stat_activity using (pid) where wait_event_type = 'Lock'`, holder).Scan(&waiting); err != nil {
			t.Fatalf("read lock waiters: %v", err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends waited on the lock, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAnEnrollmentRacingARevokeCannotLand pins the lock Create takes on the credential: a revoke
// committing while Create is enrolling under that credential (here a transaction holding the
// credential's row as RevokeCredential does, with revoked_at set) leaves Create refused
// ErrUnauthenticated and no enrollment written, never a session the revoke did not end.
func TestAnEnrollmentRacingARevokeCannotLand(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintCredential(t, svc, str("ada@example.com"), nil, "devbox")
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from launcher_credentials where id=$1 for no key update`, cred.ID); err != nil {
		t.Fatalf("lock the credential: %v", err)
	}
	if _, err := tx.Exec(ctx, `update launcher_credentials set revoked_at=now() where id=$1`, cred.ID); err != nil {
		t.Fatalf("revoke the credential: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := svc.Create(ctx, cred, Enrollment{Kind: "host", RuntimeID: "host-racing", Thumbprint: "tp-racing"})
		done <- err
	}()
	awaitLockWaiters(t, svc, tx, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the revoke: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Create racing the revoke = %v, want ErrUnauthenticated", err)
	}
	var n int
	if err := svc.Store.Pool.QueryRow(ctx, `select count(*) from enrollments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("enrollments after the race = %d (%v), want none", n, err)
	}
}

// TestARevokeEndsAnEnrollmentCreatedWhileItWaited pins the other half: RevokeCredential takes the
// credential's row before it reads the credential's enrollments, so an enrollment being inserted
// under it (here a transaction holding the row for share, as Create does, with its enrollment
// written) commits first and is ended by the revoke.
func TestARevokeEndsAnEnrollmentCreatedWhileItWaited(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintApprovedCredential(t, svc, "ada@example.com", nil, "devbox")
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from launcher_credentials where id=$1 for share`, cred.ID); err != nil {
		t.Fatalf("hold the credential for share: %v", err)
	}
	enrollment := uuid.New()
	if _, err := tx.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'host','host-waited','ada@example.com','tp-waited',$2, now() + interval '1 hour')`, enrollment, cred.ID); err != nil {
		t.Fatalf("insert the enrollment: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.RevokeCredential(ctx, cred.ID.String(), "ada@example.com") }()
	awaitLockWaiters(t, svc, tx, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the enrollment: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if _, live, err := svc.Lookup(ctx, enrollment.String()); err != nil || live {
		t.Fatalf("Lookup(the enrollment committed while the revoke waited) = live %v, %v; want ended", live, err)
	}
}

// TestTwoRevokesOfOneLoginEndItOnce pins RevokeCredential's lock on the credential's row: two
// revokes of one login that start while an enrollment holds that row for share (as Create does)
// queue on it, so the second reads the credential the first revoked and changes nothing — one
// launcher_credential.revoked row and one enrollment.revoked row, never two.
func TestTwoRevokesOfOneLoginEndItOnce(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintApprovedCredential(t, svc, "ada@example.com", nil, "devbox")
	if _, err := svc.Create(ctx, cred, Enrollment{Kind: "host", RuntimeID: "host-1", Thumbprint: "tp-host-1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from launcher_credentials where id=$1 for share`, cred.ID); err != nil {
		t.Fatalf("hold the credential for share: %v", err)
	}
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- svc.RevokeCredential(ctx, cred.ID.String(), "ada@example.com") }()
	}
	awaitLockWaiters(t, svc, tx, 2)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release the credential: %v", err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("RevokeCredential: %v", err)
		}
	}
	var credentialAudits, enrollmentAudits int
	if err := svc.Store.Pool.QueryRow(ctx, `select (select count(*) from audit where kind='launcher_credential.revoked'),
		(select count(*) from audit where kind='enrollment.revoked')`).Scan(&credentialAudits, &enrollmentAudits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if credentialAudits != 1 || enrollmentAudits != 1 {
		t.Fatalf("audit rows after two racing revokes: launcher_credential.revoked rows = %d, enrollment.revoked rows = %d; want 1 and 1", credentialAudits, enrollmentAudits)
	}
}

// TestARevokeRacingTheLaunchersOwnRevokeEndsTheSessionOnce pins RevokeCredential's lock on the
// credential's enrollments: a launcher's own Revoke of one of them that is still committing (here
// a transaction holding the enrollment's row and ending it, as Revoke does) leaves the machine
// login's revoke waiting on that row; it then finds the enrollment ended and leaves it out, so the
// session has one enrollment.revoked row, never two, and the login's audit row names no session.
func TestARevokeRacingTheLaunchersOwnRevokeEndsTheSessionOnce(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	cred := mintApprovedCredential(t, svc, "ada@example.com", nil, "devbox")
	enr, err := svc.Create(ctx, cred, Enrollment{Kind: "host", RuntimeID: "host-1", Thumbprint: "tp-host-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tx, err := svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from enrollments where id=$1 for update`, enr.ID); err != nil {
		t.Fatalf("lock the enrollment: %v", err)
	}
	if _, _, err := endEnrollment(ctx, tx, enr.ID.String(), "launcher:"+cred.ID.String(), "enrollment.revoked", "revoked by its launcher"); err != nil {
		t.Fatalf("end the enrollment as its launcher: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.RevokeCredential(ctx, cred.ID.String(), "ada@example.com") }()
	awaitLockWaiters(t, svc, tx, 1)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the launcher's revoke: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	var enrollmentAudits, named int
	if err := svc.Store.Pool.QueryRow(ctx, `select (select count(*) from audit where kind='enrollment.revoked' and enrollment_id=$1),
		(select jsonb_array_length(detail->'enrollments') from audit where kind='launcher_credential.revoked')`, enr.ID).Scan(&enrollmentAudits, &named); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if enrollmentAudits != 1 || named != 0 {
		t.Fatalf("after the race: enrollment.revoked rows = %d, sessions the login's revoke names = %d; want 1 and 0", enrollmentAudits, named)
	}
}
