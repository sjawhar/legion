package session

import (
	"testing"
	"time"
)

func TestDedupe_FirstSeen(t *testing.T) {
	d := NewDedupe(5 * time.Minute)
	if d.Check("key-1") {
		t.Fatal("first check should return false (not a duplicate)")
	}
}

func TestDedupe_SecondSeen(t *testing.T) {
	d := NewDedupe(5 * time.Minute)
	d.Check("key-1")
	if !d.Check("key-1") {
		t.Fatal("second check should return true (duplicate)")
	}
}

func TestDedupe_DifferentKeys(t *testing.T) {
	d := NewDedupe(5 * time.Minute)
	d.Check("key-1")
	if d.Check("key-2") {
		t.Fatal("different key should not be a duplicate")
	}
}

func TestDedupe_EmptyKey(t *testing.T) {
	d := NewDedupe(5 * time.Minute)
	if d.Check("") {
		t.Fatal("empty key should never be a duplicate")
	}
	if d.Check("") {
		t.Fatal("empty key should never be a duplicate even on second call")
	}
}

func TestDedupe_Expiry(t *testing.T) {
	d := NewDedupe(10 * time.Millisecond)
	d.Check("key-1")
	time.Sleep(20 * time.Millisecond)
	if d.Check("key-1") {
		t.Fatal("key should have expired after window")
	}
}
