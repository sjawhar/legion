package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// Every rewrite keeps the JSON keys of the record this build does not know.
func TestLeaseRewriteKeepsUnknownFields(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-new-build","boot":"0011223344556677","released":true,"future":{"k":[1,2]}}`)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	b.step()
	if !b.Holding() {
		t.Fatal("standby did not take the released record")
	}
	clock.advance(leaseRenewEvery)
	b.step()
	_, entry := readLease(t, kv)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &fields); err != nil {
		t.Fatalf("decode %s: %v", entry.Value(), err)
	}
	if got := string(fields["future"]); got != `{"k":[1,2]}` {
		t.Fatalf("record %s: future = %s, want the newer build's field kept", entry.Value(), got)
	}
	if got := string(fields["holder"]); got != `"pod-b"` {
		t.Fatalf("record %s: holder = %s, want pod-b", entry.Value(), got)
	}
}

// formerBootNames lists the boots of a record's former_boots, oldest first.
func formerBootNames(record leaseRecord) []string {
	names := make([]string, 0, len(record.FormerBoots))
	for _, former := range record.FormerBoots {
		names = append(names, former.Boot)
	}
	return names
}

func sameFormerBoot(x, y formerBoot) bool {
	return x.Boot == y.Boot && x.Stopped.Equal(y.Stopped)
}

// An acquisition over an expired record and one over a released record each add that record's
// boot to former_boots, stopped at the acquiring write's timestamp, and carry the record's own
// former_boots on, so a chain A -> B -> C names A and B. Renewals and a release leave it as it
// stands, and FormerHolder reads it.
func TestLeaseFormerBootsAreWrittenOnEveryTakeOverAndCarriedAlongAChain(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	c := testLease(kv, "pod-c", productionLeaseConfig(), clock.now)

	a.step()
	if record, _ := readLease(t, kv); len(record.FormerBoots) != 0 {
		t.Fatalf("former_boots over an absent record = %v, want none", formerBootNames(record))
	}
	b.step()
	clock.advance(leaseTTL)
	b.step()
	if !b.Holding() {
		t.Fatal("pod-b did not take the expired record")
	}
	expired, _ := readLease(t, kv)
	if len(expired.FormerBoots) != 1 || expired.FormerBoots[0].Boot != a.boot || !expired.FormerBoots[0].Stopped.Equal(expired.AcquiredAt) {
		t.Fatalf("former_boots after the expired take-over = %+v (acquired_at %v), want pod-a's boot %s stopped at acquired_at",
			expired.FormerBoots, expired.AcquiredAt, a.boot)
	}
	if !b.FormerHolder(a.boot) || b.FormerHolder(b.boot) || b.FormerHolder(c.boot) {
		t.Fatalf("pod-b's FormerHolder: pod-a %v, pod-b %v, pod-c %v; want pod-a alone",
			b.FormerHolder(a.boot), b.FormerHolder(b.boot), b.FormerHolder(c.boot))
	}

	for range 3 {
		clock.advance(leaseRenewEvery)
		b.step()
	}
	c.step()
	if err := b.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	released, _ := readLease(t, kv)
	if !released.Released || len(released.FormerBoots) != 1 || !sameFormerBoot(released.FormerBoots[0], expired.FormerBoots[0]) {
		t.Fatalf("released record's former_boots = %+v, want the acquiring write's %+v unchanged by renewals and the release",
			released.FormerBoots, expired.FormerBoots)
	}

	c.step()
	if !c.Holding() {
		t.Fatal("pod-c did not take the released record")
	}
	chained, _ := readLease(t, kv)
	if got := formerBootNames(chained); len(got) != 2 || got[0] != a.boot || got[1] != b.boot {
		t.Fatalf("former_boots after A -> B -> C = %v, want [%s %s]", got, a.boot, b.boot)
	}
	if !chained.FormerBoots[0].Stopped.Equal(expired.FormerBoots[0].Stopped) || !chained.FormerBoots[1].Stopped.Equal(chained.AcquiredAt) {
		t.Fatalf("former_boots after A -> B -> C = %+v (acquired_at %v), want pod-a's stop kept and pod-b's at acquired_at",
			chained.FormerBoots, chained.AcquiredAt)
	}
	if !c.FormerHolder(a.boot) || !c.FormerHolder(b.boot) || c.FormerHolder(c.boot) {
		t.Fatal("pod-c's FormerHolder does not name pod-a and pod-b alone")
	}
}

// An acquisition keeps only the former boots that stopped less than leaseFormerBootAge before
// it, never names its own boot, and keeps the newest leaseFormerBootsMax.
func TestLeaseFormerBootsArePrunedByAgeAndCount(t *testing.T) {
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	now := time.Now()
	stopped := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339Nano) }

	putLease(t, kv, fmt.Sprintf(`{"holder":"pod-old","boot":"00000000000000aa","released":true,"former_boots":[`+
		`{"boot":"00000000000000a1","stopped":%q},{"boot":%q,"stopped":%q},{"boot":"00000000000000a2","stopped":%q}]}`,
		stopped(leaseFormerBootAge+time.Minute), b.boot, stopped(20*time.Second), stopped(10*time.Second)))
	b.step()
	if !b.Holding() {
		t.Fatal("pod-b did not take the released record")
	}
	record, _ := readLease(t, kv)
	if got := formerBootNames(record); len(got) != 2 || got[0] != "00000000000000a2" || got[1] != "00000000000000aa" {
		t.Fatalf("former_boots = %v, want [00000000000000a2 00000000000000aa]: the old entry and pod-b's own boot pruned", got)
	}
	if b.FormerHolder("00000000000000a1") {
		t.Fatal("FormerHolder names a boot that stopped longer than leaseFormerBootAge before the take-over")
	}

	c := testLease(kv, "pod-c", productionLeaseConfig(), clock.now)
	entries := make([]string, 0, 20)
	for i := range 20 {
		entries = append(entries, fmt.Sprintf(`{"boot":"%016x","stopped":%q}`, i, stopped(time.Duration(20-i)*time.Second)))
	}
	putLease(t, kv, `{"holder":"pod-old","boot":"00000000000000bb","released":true,"former_boots":[`+strings.Join(entries, ",")+`]}`)
	c.step()
	if !c.Holding() {
		t.Fatal("pod-c did not take the released record")
	}
	record, _ = readLease(t, kv)
	got := formerBootNames(record)
	if len(got) != leaseFormerBootsMax || got[0] != fmt.Sprintf("%016x", 20-leaseFormerBootsMax+1) || got[len(got)-1] != "00000000000000bb" {
		t.Fatalf("former_boots = %v, want the newest %d: entries %d to 19, then the released holder's boot",
			got, leaseFormerBootsMax, 20-leaseFormerBootsMax+1)
	}
}

// A holder's own lapse and resume is no take-over: it adds nothing to former_boots and keeps the
// list it acquired with.
func TestLeaseFormerBootsAreNotChangedByAResumeAfterALapse(t *testing.T) {
	kv := leaseBucket(t)
	var fail atomic.Bool
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-old","boot":"00000000000000aa","released":true}`)
	a := testLease(bus.KeyValue{KeyValue: faultyLeaseKV{KeyValue: kv.KeyValue, fail: &fail}}, "pod-a", productionLeaseConfig(), clock.now)
	a.step()
	acquired, _ := readLease(t, kv)
	if got := formerBootNames(acquired); len(got) != 1 || got[0] != "00000000000000aa" {
		t.Fatalf("former_boots after the take-over = %v, want [00000000000000aa]", got)
	}

	fail.Store(true)
	for a.Holding() {
		clock.advance(leaseRenewEvery)
		a.step()
	}
	fail.Store(false)
	clock.advance(leaseRenewEvery)
	a.step()
	if status := a.Status(); !status.Holding || status.Term != 2 {
		t.Fatalf("status after the outage = %+v, want term 2 held", status)
	}
	resumed, _ := readLease(t, kv)
	if len(resumed.FormerBoots) != 1 || !sameFormerBoot(resumed.FormerBoots[0], acquired.FormerBoots[0]) {
		t.Fatalf("former_boots after the resume = %+v, want the acquisition's %+v", resumed.FormerBoots, acquired.FormerBoots)
	}
	if a.FormerHolder(a.boot) {
		t.Fatal("a resumed holder names its own boot a former holder")
	}
}
