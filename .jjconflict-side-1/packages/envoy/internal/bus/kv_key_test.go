package bus

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A KV call builds its subjects from its key, which nats.go checks only for the characters a key may
// hold. A key long enough to take the subject past the server's protocol line would close the
// connection every subscription and watcher of the client runs on, and one holding an empty token
// (`a..b`, which nats.go allows) names a subject no stream matches; one outside nats.go's key
// alphabet (`ses:bad`) nats.go refuses itself. The handle a bucket opens with refuses each as the
// ErrRefused it is, naming the key or its size, before sending anything, on every call that builds a
// subject from its key, and the key outside the alphabet on a get by revision too, which builds none,
// whether the open created the bucket or found it.
func TestAKeyValueHandleRefusesAKeyNATSWouldRefuse(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	created, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: "kv_key_check", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue("kv_key_check") })
	found, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: "kv_key_check", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("find bucket: %v", err)
	}
	opened, err := OpenKeyValue(js, "kv_key_check")
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	for i, open := range []struct {
		name string
		kv   nats.KeyValue
	}{{"created", created}, {"found", found}, {"opened", opened}} {
		t.Run(open.name, func(t *testing.T) { requireKeysChecked(t, client, open.kv, string(rune('a'+i))) })
	}
}

// requireKeysChecked fails the test unless kv refuses a key NATS would refuse on every call that
// takes one and still takes the longest key that fits, the control keys spelled with letter.
func requireKeysChecked(t *testing.T, client *Client, kv nats.KeyValue, letter string) {
	t.Helper()

	calls := map[string]func(key string) error{
		"Get":       func(key string) error { _, err := kv.Get(key); return err },
		"Put":       func(key string) error { _, err := kv.Put(key, []byte("v")); return err },
		"PutString": func(key string) error { _, err := kv.PutString(key, "v"); return err },
		"Create":    func(key string) error { _, err := kv.Create(key, []byte("v")); return err },
		"Update":    func(key string) error { _, err := kv.Update(key, []byte("v"), 1); return err },
		"Delete":    func(key string) error { return kv.Delete(key) },
		"Purge":     func(key string) error { return kv.Purge(key) },
		"History":   func(key string) error { _, err := kv.History(key); return err },
		"Watch": func(key string) error {
			watcher, err := kv.Watch(key)
			if err == nil {
				_ = watcher.Stop()
			}
			return err
		},
		"WatchFiltered": func(key string) error {
			watcher, err := kv.WatchFiltered([]string{"fits", key})
			if err == nil {
				_ = watcher.Stop()
			}
			return err
		},
	}
	// A get by revision names the revision, not the key, in its subject, so only nats.go's alphabet
	// check refuses a key there.
	everyCall := maps.Clone(calls)
	everyCall["GetRevision"] = func(key string) error { _, err := kv.GetRevision(key, 1); return err }
	for _, tc := range []struct {
		name string
		key  string
		want error
		says string
		// also is nats.go's own error, which a caller that asks for it still finds.
		also error
		// calls are the calls that refuse key.
		calls map[string]func(key string) error
	}{
		{"a key past the server's protocol line", strings.Repeat("k", 5000), ErrTooLarge, "a key of 5000 bytes", nil, calls},
		{"a key holding an empty token", "sess..x", ErrInvalidSubject, `"sess..x"`, nil, calls},
		{"a key outside nats.go's key alphabet", "ses:bad", ErrInvalidKey, `"ses:bad"`, nats.ErrInvalidKey, everyCall},
	} {
		for name, call := range tc.calls {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				err := call(tc.key)
				if !errors.Is(err, tc.want) || !errors.Is(err, ErrRefused) {
					t.Fatalf("%s error = %v, want %v, an ErrRefused", name, err, tc.want)
				}
				if tc.also != nil && !errors.Is(err, tc.also) {
					t.Fatalf("%s error = %v, want it to match %v too", name, err, tc.also)
				}
				if !strings.Contains(err.Error(), tc.says) {
					t.Fatalf("%s error = %q, want it to say %q", name, err, tc.says)
				}
				if !client.Conn.IsConnected() {
					t.Fatalf("the refusal left the connection %v, want it connected", client.Conn.Status())
				}
			})
		}
	}
	// The control: the same handle still takes a key that fits, the longest one included.
	longest := strings.Repeat(letter, maxSubjectBytes-watchOverhead("kv_key_check"))
	for _, key := range []string{"fits-" + letter, longest} {
		revision, err := kv.Put(key, []byte("v"))
		if err != nil {
			t.Fatalf("put %d-byte key: %v", len(key), err)
		}
		if _, err := kv.Get(key); err != nil {
			t.Fatalf("get %d-byte key: %v", len(key), err)
		}
		if entry, err := kv.GetRevision(key, revision); err != nil || entry.Revision() != revision {
			t.Fatalf("get %d-byte key at revision %d = %v; want that revision", len(key), revision, err)
		}
		history, err := kv.History(key)
		if err != nil || len(history) != 1 {
			t.Fatalf("history of %d-byte key = %d entries, %v; want one", len(key), len(history), err)
		}
	}
	if !client.Conn.IsConnected() {
		t.Fatalf("the longest accepted key left the connection %v, want it connected", client.Conn.Status())
	}
}

// A key already in a bucket stays usable whatever this build would now refuse to write: a listener
// built before the key check stored keys up to the length its own direct get could read. A write
// of such a key is refused, since a key written now must stay readable and watchable; a read is
// refused only where the read's own subject would be too long, and every such key still lists and
// deletes, so a store can serve it or skip it, and a reaper can remove it, rather than fail.
func TestAKeyAnEarlierListenerStoredStaysListableAndDeletable(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	raw, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "kv_legacy", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue("kv_legacy") })
	readable, unreadable := testnats.LegacyKeys("kv_legacy", "l")
	for _, key := range []string{readable, unreadable} {
		if _, err := raw.Put(key, []byte("legacy")); err != nil {
			t.Fatalf("store the %d-byte key as an earlier listener did: %v", len(key), err)
		}
	}
	kv, err := OpenKeyValue(js, "kv_legacy")
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}

	for _, key := range []string{readable, unreadable} {
		if _, err := kv.Put(key, []byte("again")); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("put of the stored %d-byte key = %v, want ErrTooLarge", len(key), err)
		}
	}
	entry, err := kv.Get(readable)
	if err != nil || string(entry.Value()) != "legacy" {
		t.Fatalf("get of the stored %d-byte key = %v, want its value", len(readable), err)
	}
	if _, err := kv.Get(unreadable); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("get of the stored %d-byte key = %v, want ErrTooLarge", len(unreadable), err)
	}
	keys, err := kv.Keys()
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys = %d, %v; want both stored keys", len(keys), err)
	}
	for _, key := range []string{unreadable, readable} {
		if err := kv.Delete(key); err != nil {
			t.Fatalf("delete of the stored %d-byte key: %v", len(key), err)
		}
	}
	if keys, err := kv.Keys(); !errors.Is(err, nats.ErrNoKeysFound) {
		t.Fatalf("keys after both deletes = %d, %v; want none", len(keys), err)
	}
	if !client.Conn.IsConnected() {
		t.Fatalf("the stored keys left the connection %v, want it connected", client.Conn.Status())
	}
}
