package requests

import (
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// newAirlockFixture is newFixture with an airlock behind it: the policy's loader reads each
// secret's entry fingerprint out of digests, the map the caller keeps, so a test moves one entry
// by writing to that map and calling reloadAirlock. It is a fixture of its own because
// newFixture's loader has no rules file at all, which is the deployment every other row of this
// package is about.
func newAirlockFixture(t *testing.T, digests map[string]string) (m *Machine, enrollmentID string, requesterKey *ecdsa.PrivateKey) {
	t.Helper()
	st := storetest.Open(t)
	local := secrets.NewLocal(fixtureSecrets()...)
	enrollmentID, requesterKey = newEnrollment(t, st, "session", "box-a-"+t.Name(), new(fixtureOperator), nil)

	loader := policytest.Loader(local, fixtureService)
	loader.AirlockDigest = func(name string) string { return digests[name] }
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("policy.NewCurrent: %v", err)
	}
	m = &Machine{
		Store:             st,
		Policy:            current,
		Secrets:           policytest.Store(local),
		PendingTTL:        12 * time.Hour,
		Audience:          testAudience,
		Skew:              time.Minute,
		Replay:            replayer(st),
		ServicesByAccount: map[string]string{fixtureSubject: fixtureService},
	}
	m.Chain = NewChainVerifier(st, testAudience, time.Minute)
	return m, enrollmentID, requesterKey
}

// reloadAirlock loads the policy again, as the pod the rules file's change rolls in does at its
// own first load: the broker reads that file once at start, so a changed entry reaches a running
// broker no other way.
func reloadAirlock(t *testing.T, m *Machine) {
	t.Helper()
	if err := m.Policy.Refresh(t.Context()); err != nil {
		t.Fatalf("policy refresh: %v", err)
	}
}

// recordedDigests reads the airlock fingerprint request requestID recorded for each name it
// names.
func recordedDigests(t *testing.T, m *Machine, requestID string) map[string]string {
	t.Helper()
	rows, err := m.Store.Pool.Query(t.Context(), `select name, airlock_digest from request_secrets where request_id=$1`, requestID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	recorded := map[string]string{}
	for rows.Next() {
		var name, digest string
		if err := rows.Scan(&name, &digest); err != nil {
			t.Fatal(err)
		}
		recorded[name] = digest
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// TestARequestRecordsEachNamesAirlockDigest: a request freezes the fingerprint of every name it
// names, the airlock's entry for one and the empty string for a name the rules do not name, which
// is what the release compares against.
func TestARequestRecordsEachNamesAirlockDigest(t *testing.T) {
	digests := map[string]string{"AUTO_TOKEN": "entry-one"}
	m, enr, key := newAirlockFixture(t, digests)
	request, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read both", "AUTO_TOKEN", "DEEL_API_KEY"), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	recorded := recordedDigests(t, m, request.ID)
	if recorded["AUTO_TOKEN"] != "entry-one" {
		t.Errorf("AUTO_TOKEN recorded digest = %q, want entry-one", recorded["AUTO_TOKEN"])
	}
	if recorded["DEEL_API_KEY"] != "" {
		t.Errorf("DEEL_API_KEY recorded digest = %q, want none: the rules do not name it", recorded["DEEL_API_KEY"])
	}
}

// TestAChangedAirlockEntryEndsTheGrantsItAffects: a grant whose released name's entry changed
// ends for good at its next release, naming the secret in its grant.revoked audit row, and
// putting the entry back does not bring it back. A grant of a name whose entry did not change
// goes on releasing, even though the change moved the policy version both grants were decided
// under.
func TestAChangedAirlockEntryEndsTheGrantsItAffects(t *testing.T) {
	digests := map[string]string{"AUTO_TOKEN": "entry-one", "SHARED_KEY": "shared-one"}
	m, enr, key := newAirlockFixture(t, digests)
	moved, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the token", "AUTO_TOKEN"), "")
	if err != nil || moved.GrantID == nil {
		t.Fatalf("Create(AUTO_TOKEN) = %+v, %v; want a grant", moved, err)
	}
	// SHARED_KEY is human tier, so its grant comes of an approval; its entry never moves.
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the shared key", "SHARED_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending", pending, err)
	}
	kept, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
	if err != nil || kept.GrantID == "" {
		t.Fatalf("ApplyDecision(SHARED_KEY) = %+v, %v", kept, err)
	}

	digests["AUTO_TOKEN"] = "entry-two"
	reloadAirlock(t, m)
	if release, err := m.Values(t.Context(), *moved.GrantID, enr, nil); err == nil || release.Values != nil {
		t.Fatalf("Values after the entry changed = %v, %v; want the grant ended", release.Values, err)
	}
	if release, err := m.Values(t.Context(), kept.GrantID, enr, nil); err != nil || release.Values["SHARED_KEY"] != "shared-v1" {
		t.Fatalf("Values of the grant whose entry did not change = %v, %v; want the value", release.Values, err)
	}

	digests["AUTO_TOKEN"] = "entry-one"
	reloadAirlock(t, m)
	if release, err := m.Values(t.Context(), *moved.GrantID, enr, nil); err == nil || release.Values != nil {
		t.Fatalf("Values once the entry is back = %v, %v; want the grant to stay ended", release.Values, err)
	}
	if by, audits := endedForGood(t, m, *moved.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
	}
	var reason string
	if err := m.Store.Pool.QueryRow(t.Context(), `select detail->>'reason' from audit where kind='grant.revoked' and grant_id=$1`, *moved.GrantID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if want := ErrGrantNotLive.Error() + ": AUTO_TOKEN's airlock entry changed since this grant was decided"; reason != want {
		t.Fatalf("grant.revoked reason = %q, want %q", reason, want)
	}
	if by, _ := endedForGood(t, m, kept.GrantID); by != "" {
		t.Fatalf("the grant whose entry did not change was revoked by %q; want it live", by)
	}
}

// TestARequestApprovedAcrossARulesChangeIsReleasedUnderTheEntryItWasDecidedUnder: a request
// waits on its person for up to half a day, and the rules file can roll while it waits. What a
// release compares each name against is the entry that name was decided under, and for an
// approved request the decision is the approval: so the grant a person has just made releases,
// where recording the entry at creation alone ended it on its first release with a reason that
// was not true. An entry that moves after the decision still ends the grant, which is what the
// fingerprint is there for.
func TestARequestApprovedAcrossARulesChangeIsReleasedUnderTheEntryItWasDecidedUnder(t *testing.T) {
	digests := map[string]string{"SHARED_KEY": "entry-one"}
	m, enr, key := newAirlockFixture(t, digests)
	// SHARED_KEY is shared and human tier, so its grant comes of a person's approval.
	pending, err := m.Create(t.Context(), enr, signRequest(t, m, key, "read the shared key", "SHARED_KEY"), "")
	if err != nil || pending.RecordID == nil {
		t.Fatalf("Create(SHARED_KEY) = %+v, %v; want pending", pending, err)
	}

	// The rules roll while the card sits on the approver's page.
	digests["SHARED_KEY"] = "entry-two"
	reloadAirlock(t, m)
	decided, err := m.ApplyDecision(t.Context(), *pending.RecordID, true, fixtureOperator)
	if err != nil || decided.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decided, err)
	}
	if got := recordedDigests(t, m, pending.ID)["SHARED_KEY"]; got != "entry-two" {
		t.Fatalf("the approved request records the digest %q; want entry-two, the entry it was decided under", got)
	}
	if release, err := m.Values(t.Context(), decided.GrantID, enr, nil); err != nil || release.Values["SHARED_KEY"] != "shared-v1" {
		t.Fatalf("Values of the grant the approval made = %v, %v; want the value: the approval was given under this entry", release.Values, err)
	}
	if by, _ := endedForGood(t, m, decided.GrantID); by != "" {
		t.Fatalf("the grant the approval made was revoked by %q; want it live", by)
	}

	// An entry that moves after the decision ends the grant it was decided under.
	digests["SHARED_KEY"] = "entry-three"
	reloadAirlock(t, m)
	if release, err := m.Values(t.Context(), decided.GrantID, enr, nil); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values after the entry changed = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
	}
	if by, audits := endedForGood(t, m, decided.GrantID); by != GrantEndedActor || audits != 1 {
		t.Fatalf("grant revoked_by = %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
	}
}
