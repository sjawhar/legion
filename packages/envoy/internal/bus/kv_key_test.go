package bus

import (
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A KV call builds its subjects from its key, which nats.go checks only for the characters a key may
// hold. A key long enough to take the subject past the server's protocol line would close the
// connection every subscription and watcher of the client runs on, and one holding an empty token
// (`a..b`, which nats.go allows) names a subject no stream matches. The handle a bucket opens with
// refuses both as the ErrRefused they are, before sending anything, whichever call carries the key,
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
		"Get":         func(key string) error { _, err := kv.Get(key); return err },
		"GetRevision": func(key string) error { _, err := kv.GetRevision(key, 1); return err },
		"Put":         func(key string) error { _, err := kv.Put(key, []byte("v")); return err },
		"PutString":   func(key string) error { _, err := kv.PutString(key, "v"); return err },
		"Create":      func(key string) error { _, err := kv.Create(key, []byte("v")); return err },
		"Update":      func(key string) error { _, err := kv.Update(key, []byte("v"), 1); return err },
		"Delete":      func(key string) error { return kv.Delete(key) },
		"Purge":       func(key string) error { return kv.Purge(key) },
		"History":     func(key string) error { _, err := kv.History(key); return err },
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
	for _, tc := range []struct {
		name string
		key  string
		want error
		says string
	}{
		{"a key past the server's protocol line", strings.Repeat("k", 5000), ErrTooLarge, "a key of 5000 bytes"},
		{"a key holding an empty token", "sess..x", ErrInvalidSubject, `"sess..x"`},
	} {
		for name, call := range calls {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				err := call(tc.key)
				if !errors.Is(err, tc.want) || !errors.Is(err, ErrRefused) {
					t.Fatalf("%s error = %v, want %v, an ErrRefused", name, err, tc.want)
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
	longest := strings.Repeat(letter, maxSubjectBytes-kvKeyOverhead("kv_key_check"))
	for _, key := range []string{"fits-" + letter, longest} {
		if _, err := kv.Put(key, []byte("v")); err != nil {
			t.Fatalf("put %d-byte key: %v", len(key), err)
		}
		if _, err := kv.Get(key); err != nil {
			t.Fatalf("get %d-byte key: %v", len(key), err)
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
