package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// TestKeyedAllowsABurstThenOnePerEveryForEachKey pins the bucket: Burst requests at once, then one
// per Every, and one key's requests never spend another's.
func TestKeyedAllowsABurstThenOnePerEveryForEachKey(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Minute, Burst: 2})
	if !k.AllowAt("a", t0) || !k.AllowAt("a", t0) {
		t.Fatal("a burst of 2 was refused")
	}
	if k.AllowAt("a", t0) {
		t.Fatal("a third request inside the burst was allowed")
	}
	if !k.AllowAt("b", t0) {
		t.Fatal("a's requests spent b's bucket")
	}
	if k.AllowAt("a", t0.Add(time.Minute-time.Second)) {
		t.Fatal("a's bucket refilled before Every passed")
	}
	if !k.AllowAt("a", t0.Add(time.Minute)) {
		t.Fatal("a's bucket did not refill once Every passed")
	}
}

// TestKeyedDropsOnlyFullBucketsAtItsBound pins the memory bound: once maxBuckets keys hold
// buckets, a new key drops the full ones, and a key that spent its bucket keeps it, so spraying
// fresh keys never hands a flooding key a fresh burst.
func TestKeyedDropsOnlyFullBucketsAtItsBound(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Minute, Burst: 1})
	for i := range maxBuckets - 1 {
		k.AllowAt(fmt.Sprintf("idle-%d", i), t0)
	}
	t1 := t0.Add(time.Minute) // every idle bucket has refilled to its burst
	if !k.AllowAt("flood", t1) {
		t.Fatal("flood's first request was refused")
	}
	if !k.AllowAt("newcomer", t1) {
		t.Fatal("a new key was refused at the bound")
	}
	if k.AllowAt("flood", t1) {
		t.Fatal("flood's spent bucket was dropped at the bound, giving it a fresh burst")
	}
	if n := len(k.buckets); n != 2 {
		t.Fatalf("kept %d buckets past the bound, want the 2 that are not full", n)
	}
}

// TestAKeyFloodingItsOwnLimitSpendsNothingShared pins AllowBothAt's first property: a request its
// key's bucket refuses takes nothing from the shared bucket, so one key flooding past its own
// limit leaves the shared bucket to every other key.
func TestAKeyFloodingItsOwnLimitSpendsNothingShared(t *testing.T) {
	perKey := Limit{Every: time.Hour, Burst: 1}
	k, shared := NewKeyed(perKey), NewBucket(Limit{Every: time.Hour, Burst: 2})
	if _, ok := k.AllowBothAt("flood", shared, t0); !ok {
		t.Fatal("flood's first request was refused")
	}
	for i := range 5 {
		if refused, ok := k.AllowBothAt("flood", shared, t0); ok || refused != perKey {
			t.Fatalf("flood's request %d = %+v, %v; want refused by its own limit", i+2, refused, ok)
		}
	}
	if _, ok := k.AllowBothAt("other", shared, t0); !ok {
		t.Fatal("another key's first request was refused: the flood spent the shared bucket")
	}
}

// TestASharedRefusalSpendsNothingOfTheKeys pins AllowBothAt's second property: a request the
// shared bucket refuses takes nothing from its key's bucket, so a caller retrying through a flood
// of others is not refused by its own limit once the shared bucket refills.
func TestASharedRefusalSpendsNothingOfTheKeys(t *testing.T) {
	all := Limit{Every: time.Second, Burst: 1}
	k, shared := NewKeyed(Limit{Every: time.Hour, Burst: 1}), NewBucket(all)
	if _, ok := k.AllowBothAt("other", shared, t0); !ok {
		t.Fatal("other's first request was refused")
	}
	for i := range 3 {
		if refused, ok := k.AllowBothAt("caller", shared, t0); ok || refused != all {
			t.Fatalf("caller's request %d = %+v, %v; want refused by the shared limit", i+1, refused, ok)
		}
	}
	if _, ok := k.AllowBothAt("caller", shared, t0.Add(time.Second)); !ok {
		t.Fatal("caller was refused once the shared bucket refilled: the shared refusals spent its own bucket")
	}
}

// TestAGivenBackRequestCanBeTakenAgain pins TakeAt's give-back: a request given back is in its
// bucket again, even when the give-back comes after another caller's take, and the other caller's
// request stays taken.
func TestAGivenBackRequestCanBeTakenAgain(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Minute, Burst: 2})
	giveBack, ok := k.TakeAt("a", t0)
	if !ok {
		t.Fatal("a's first take was refused")
	}
	if _, ok := k.TakeAt("a", t0.Add(time.Second)); !ok {
		t.Fatal("a's second take inside the burst was refused")
	}
	giveBack()
	if _, ok := k.TakeAt("a", t0.Add(2*time.Second)); !ok {
		t.Fatal("the request given back was not in a's bucket again")
	}
	if k.AllowAt("a", t0.Add(3*time.Second)) {
		t.Fatal("a fourth request was allowed: the give-back returned more than the one request it took")
	}
}

// TestARefusedTakeSpendsNothing pins TakeAt's refusal: a take its bucket refuses answers no
// give-back and spends nothing, so the bucket refills on its own schedule as AllowAt's does.
func TestARefusedTakeSpendsNothing(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Minute, Burst: 1})
	if _, ok := k.TakeAt("a", t0); !ok {
		t.Fatal("a's first take was refused")
	}
	for i := range 3 {
		if giveBack, ok := k.TakeAt("a", t0.Add(time.Duration(i)*time.Second)); ok || giveBack != nil {
			t.Fatalf("a's take %d inside its empty bucket = %v, want refused with no give-back", i+2, ok)
		}
	}
	if _, ok := k.TakeAt("a", t0.Add(time.Minute)); !ok {
		t.Fatal("a's bucket did not refill once Every passed: the refused takes spent it")
	}
}

// TestSetRatesAtKeepsEachKeysTokensAndSlowsItsRefill pins what a rate change does to a bucket a
// caller already spent from: the tokens it holds stay, and only the rate it gains them back at
// moves. The broker rewrites these rates when its replica count changes
// (api.RereadRate.SetReplicas), and a rewrite that refilled every bucket would hand every
// caller a fresh burst at each change of the fleet's size.
func TestSetRatesAtKeepsEachKeysTokensAndSlowsItsRefill(t *testing.T) {
	k := NewKeyed(Limit{Every: time.Second, Burst: 2})
	// A depth no row here can reach, so the only refusal below is the per-key one.
	free := Limit{Every: time.Millisecond, Burst: 1000}
	shared := NewBucket(free)
	if _, ok := k.AllowBothAt("a", shared, t0); !ok {
		t.Fatal("a's first request was refused")
	}
	slower := Limit{Every: 10 * time.Second, Burst: 2}
	k.SetRatesAt(t0, slower, shared, free)
	if got := k.Every(); got != slower.Every {
		t.Fatalf("Every() = %s after the change, want %s", got, slower.Every)
	}
	if _, ok := k.AllowBothAt("a", shared, t0); !ok {
		t.Fatal("the request a's bucket still held was refused: the rate change emptied it")
	}
	if refused, ok := k.AllowBothAt("a", shared, t0.Add(time.Second)); ok || refused != slower {
		t.Fatalf("a's request one second on = %+v, %v; want refused by the new rate, which gives one back every %s", refused, ok, slower.Every)
	}
	if _, ok := k.AllowBothAt("a", shared, t0.Add(slower.Every)); !ok {
		t.Fatal("a's bucket did not refill once the new Every passed")
	}
	if _, ok := k.AllowBothAt("b", shared, t0.Add(slower.Every)); !ok {
		t.Fatal("a key first seen after the change was refused its burst")
	}
}

// TestSetRatesAtMovesTheSharedBucketAndItsRefusal pins the same for the bucket every key shares,
// and that a refusal names the rate that bucket now refills at: Retry-After is read off the
// limit AllowBothAt answers (api.refuseReread), so a stale limit there would tell a caller to
// come back while the bucket is still empty.
func TestSetRatesAtMovesTheSharedBucketAndItsRefusal(t *testing.T) {
	// A per-key depth no row here can reach, so the only refusal below is the shared one.
	free := Limit{Every: time.Millisecond, Burst: 1000}
	k, shared := NewKeyed(free), NewBucket(Limit{Every: time.Second, Burst: 1})
	if _, ok := k.AllowBothAt("a", shared, t0); !ok {
		t.Fatal("the shared bucket refused the first request of its burst")
	}
	slower := Limit{Every: 5 * time.Second, Burst: 1}
	k.SetRatesAt(t0, free, shared, slower)
	if refused, ok := k.AllowBothAt("a", shared, t0.Add(time.Second)); ok || refused != slower {
		t.Fatalf("a request one second on = %+v, %v; want refused by the new shared rate of one every %s", refused, ok, slower.Every)
	}
	if _, ok := k.AllowBothAt("a", shared, t0.Add(slower.Every)); !ok {
		t.Fatal("the shared bucket did not refill once its new Every passed")
	}
}
