package bytelru

import (
	"fmt"
	"testing"
)

// The count the cache keeps is the bytes the list actually holds, and it never passes the limit,
// however many values of however many sizes are handed to it. The sizes below are the dashboard
// assets' (routes/assets_test.go): a whole 8 MiB object, a byte, and several that fit a 64 MiB
// cache unevenly, so eviction runs with the list in every shape.
func TestTheCacheCountsWhatItHoldsAndNeverPassesItsLimit(t *testing.T) {
	const limit int64 = 64 << 20
	cache := New[string, []byte](limit)
	sizes := []int64{8 << 20, 3 << 20, 1, (8 << 20) - 1, 5 << 20, 1747955}
	for i := range 60 {
		cache.Put(fmt.Sprintf("chunk-%d", i), make([]byte, sizes[i%len(sizes)]))
		var held int64
		for element := cache.order.Front(); element != nil; element = element.Next() {
			held += int64(len(element.Value.(*entry[string, []byte]).value))
		}
		if held > limit || held != cache.held {
			t.Fatalf("after %d values the cache holds %d bytes and counts %d, want at most %d and the same count", i+1, held, cache.held, limit)
		}
		if len(cache.items) != cache.order.Len() {
			t.Fatalf("after %d values the map holds %d keys and the list %d entries", i+1, len(cache.items), cache.order.Len())
		}
	}
}

// The value evicted is the one used least recently, and a read counts as a use.
func TestTheCacheEvictsTheValueUsedLeastRecently(t *testing.T) {
	cache := New[string, []byte](64)
	cache.Put("first", make([]byte, 32))
	cache.Put("second", make([]byte, 32))
	if _, held := cache.Get("first"); !held {
		t.Fatal("the first value was evicted from a cache it fits in")
	}
	cache.Put("third", make([]byte, 32))
	if _, held := cache.Get("first"); !held {
		t.Error("the value read most recently was evicted")
	}
	if _, held := cache.Get("second"); held {
		t.Error("the value used least recently was kept past the limit")
	}
	if _, held := cache.Get("third"); !held {
		t.Error("the newest value was not kept")
	}
	if cache.Held() > 64 {
		t.Errorf("the cache holds %d bytes, over its 64 bound", cache.Held())
	}
}

// A value larger than the whole cache is never kept, and nothing already held is dropped for it.
func TestTheCacheKeepsNoValueLargerThanItself(t *testing.T) {
	cache := New[string, []byte](64)
	cache.Put("kept", make([]byte, 32))
	cache.Put("enormous", make([]byte, 65))
	if _, held := cache.Get("enormous"); held {
		t.Error("a value larger than the whole cache was kept")
	}
	if _, held := cache.Get("kept"); !held {
		t.Error("a value larger than the whole cache emptied it")
	}
}

// A key already held is marked used again rather than counted twice.
func TestPuttingAKeyTwiceCountsItOnce(t *testing.T) {
	cache := New[string, []byte](64)
	cache.Put("same", make([]byte, 32))
	cache.Put("same", make([]byte, 32))
	if cache.Held() != 32 {
		t.Errorf("the cache counts %d bytes for one key put twice, want 32", cache.Held())
	}
	if cache.order.Len() != 1 {
		t.Errorf("one key put twice left %d entries", cache.order.Len())
	}
}
