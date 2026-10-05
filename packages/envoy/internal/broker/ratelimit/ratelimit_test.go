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
