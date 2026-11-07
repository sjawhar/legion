package bus

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

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
	created, err := EnsureKeyValue(client.Conn, js, &nats.KeyValueConfig{Bucket: "kv_key_check", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue("kv_key_check") })
	found, err := EnsureKeyValue(client.Conn, js, &nats.KeyValueConfig{Bucket: "kv_key_check", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("find bucket: %v", err)
	}
	opened, err := OpenKeyValue(client.Conn, js, "kv_key_check")
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	for i, open := range []struct {
		name string
		kv   KeyValue
	}{{"created", created}, {"found", found}, {"opened", opened}} {
		t.Run(open.name, func(t *testing.T) { requireKeysChecked(t, client, open.kv, string(rune('a'+i))) })
	}
}

// requireKeysChecked fails the test unless kv refuses a key NATS would refuse on every call that
// takes one and still takes the longest key that fits, the control keys spelled with letter.
func requireKeysChecked(t *testing.T, client *Client, kv KeyValue, letter string) {
	t.Helper()

	calls := map[string]func(key string) error{
		"Get":       func(key string) error { _, err := kv.Get(key); return err },
		"Put":       func(key string) error { _, err := kv.Put(key, []byte("v")); return err },
		"PutString": func(key string) error { _, err := kv.PutString(key, "v"); return err },
		"Create":    func(key string) error { _, err := kv.Create(key, []byte("v")); return err },
		"Update":    func(key string) error { _, err := kv.Update(key, []byte("v"), 1); return err },
		"Delete":    func(key string) error { return kv.Delete(key) },
		"Purge":     func(key string) error { return kv.Purge(key) },
		"ReadHistory": func(key string) error {
			_, err := kv.ReadHistory(context.Background(), key)
			return err
		},
		"WatchKeys": func(key string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			watcher, err := kv.WatchKeys(ctx, key)
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
		// also is nats.go's own error, which a caller that asks for it still finds: the jetstream
		// package's own ErrInvalidKey on WatchKeys and ReadHistory, which go through the jetstream
		// handle.
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
				also := tc.also
				if also != nil && (name == "WatchKeys" || name == "ReadHistory") {
					also = jetstream.ErrInvalidKey
				}
				if also != nil && !errors.Is(err, also) {
					t.Fatalf("%s error = %v, want it to match %v too", name, err, also)
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
	// The control: the same handle still takes a key that fits, the longest one included, and
	// watches it through the jetstream handle.
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
		history, err := kv.ReadHistory(context.Background(), key)
		if err != nil || len(history) != 1 {
			t.Fatalf("history of %d-byte key = %d entries, %v; want one", len(key), len(history), err)
		}
		watchKey(t, kv, key)
	}
	if !client.Conn.IsConnected() {
		t.Fatalf("the longest accepted key left the connection %v, want it connected", client.Conn.Status())
	}
}

// A caller that holds a key from a scan of the bucket rather than from a read of it asks
// RefusesRead whether this build can read the key at all, so it must answer exactly the refusal a
// real Get answers before sending: it refuses the same keys, with the same ErrRefused classes and
// the same text, and answers nil wherever the get reaches the bucket, whether that get finds the
// key or not. nats.go's key alphabet is copied into this package (natsKeyValid) because keyValid
// is unexported there, and this case is what pins the copy: a nats.go bump that changed the
// alphabet must fail this build rather than silently reclassify stored keys.
//
// Pinning it takes a row per character of that alphabet, in both directions. A key is decided by
// kv.check first wherever its subject would be too long or would hold an empty token or
// whitespace, and natsKeyValid is reached only by the keys check passes, so a table of ordinary
// `a-z` keys and one colon would leave a bump free to drop `-`, `/`, `_`, `=` or `.`, or to add a
// character, with every row still green while RefusesRead and a real Get disagreed about stored
// keys. Each character nats.go accepts beyond letters and digits therefore has a key of its own
// here, stored in the bucket so the get it is compared against reaches it and answers its value,
// and the characters it rejects have several, each still reaching natsKeyValid rather than
// stopping at check.
func TestRefusesReadAnswersTheSameRefusalAGetOfTheKeyAnswers(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	const bucket = "kv_refuses_read"
	raw, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(bucket) })
	// The keys an earlier listener stored, which no KeyValue can write: one a direct get still
	// reads, and one past a direct get's bound but inside a delete's.
	readable, unreadable := testnats.LegacyKeys(bucket, "r")
	for _, key := range []string{readable, unreadable} {
		if _, err := raw.Put(key, []byte("legacy")); err != nil {
			t.Fatalf("store the %d-byte key as an earlier listener did: %v", len(key), err)
		}
	}
	kv, err := OpenKeyValue(client.Conn, js, bucket)
	if err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	// The keys whose character the alphabet accepts are stored, so the get each row compares
	// RefusesRead against reaches the bucket and answers a value rather than ErrKeyNotFound.
	for _, key := range []string{"fits-r", "ses/sub", "ses_sub", "ses=sub", "ses.sub", "0123456789"} {
		if _, err := kv.Put(key, []byte("v")); err != nil {
			t.Fatalf("put the %q key, which nats.go's alphabet accepts: %v", key, err)
		}
	}

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"a key past the server's protocol line", strings.Repeat("k", 5000)},
		{"a key holding an empty token", "sess..x"},
		{"a key outside nats.go's key alphabet", "ses:bad"},
		{"an ordinary key", "fits-r"},
		{"a stored key a direct get still reads", readable},
		{"a stored key past a direct get's bound", unreadable},
		// One row per character nats.go accepts beyond a letter or a digit, so a bump that
		// dropped one of them fails here rather than reclassifying the keys holding it.
		{"a key holding a hyphen", "fits-r"},
		{"a key holding a slash", "ses/sub"},
		{"a key holding an underscore", "ses_sub"},
		{"a key holding an equals sign", "ses=sub"},
		{"a key holding a dot", "ses.sub"},
		{"a key of digits", "0123456789"},
		// And the other direction, keys check passes and the alphabet refuses, so a bump that
		// widened it fails here too.
		{"a key holding a plus", "ses+bad"},
		{"a key holding a wildcard", "ses*bad"},
		{"a key holding a token wildcard", "ses>bad"},
		{"a key holding a percent sign", "ses%bad"},
		{"a key holding a letter outside ASCII", "sés"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answered := kv.RefusesRead(tc.key)
			_, err := kv.Get(tc.key)
			refused := errors.Is(err, ErrRefused)
			if err != nil && !refused && !errors.Is(err, nats.ErrKeyNotFound) {
				t.Fatalf("get of the %d-byte key = %v; want its value, a refusal or ErrKeyNotFound", len(tc.key), err)
			}
			if refused != (answered != nil) {
				t.Fatalf("RefusesRead = %v, get = %v; want RefusesRead to refuse exactly when the get does", answered, err)
			}
			if !refused {
				return
			}
			for _, class := range []error{ErrRefused, ErrTooLarge, ErrInvalidSubject, ErrInvalidKey, nats.ErrInvalidKey} {
				if errors.Is(answered, class) != errors.Is(err, class) {
					t.Fatalf("RefusesRead = %v and get = %v disagree on %v", answered, err, class)
				}
			}
			if answered.Error() != err.Error() {
				t.Fatalf("RefusesRead says %q, the get says %q; want the same refusal", answered, err)
			}
		})
	}
	if !client.Conn.IsConnected() {
		t.Fatalf("the refusals left the connection %v, want it connected", client.Conn.Status())
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
	kv, err := OpenKeyValue(client.Conn, js, "kv_legacy")
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
	if keys := listedKeys(t, kv); len(keys) != 2 {
		t.Fatalf("keys = %d; want both stored keys", len(keys))
	}
	for _, key := range []string{unreadable, readable} {
		if err := kv.Delete(key); err != nil {
			t.Fatalf("delete of the stored %d-byte key: %v", len(key), err)
		}
	}
	if keys := listedKeys(t, kv); len(keys) != 0 {
		t.Fatalf("keys after both deletes = %d; want none", len(keys))
	}
	if !client.Conn.IsConnected() {
		t.Fatalf("the stored keys left the connection %v, want it connected", client.Conn.Status())
	}
}

// listedKeys lists the keys kv's bucket holds through one watch of every key on kv's jetstream
// handle, delete markers skipped, as a store's listing scan does (kvwatch.ScanExistingKeys, which
// this package cannot import).
func listedKeys(t *testing.T, kv KeyValue) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher, err := kv.WatchKeys(ctx, jetstream.AllKeys, jetstream.MetaOnly(), jetstream.IgnoreDeletes())
	if err != nil {
		t.Fatalf("watch every key: %v", err)
	}
	defer func() { _ = watcher.Stop() }()
	var keys []string
	for {
		select {
		case entry, open := <-watcher.Updates():
			if !open {
				t.Fatalf("the watch of every key ended after %d keys, before its scan's nil", len(keys))
			}
			if entry == nil {
				return keys
			}
			keys = append(keys, entry.Key())
		case <-time.After(10 * time.Second):
			t.Fatalf("the watch of every key delivered %d keys and no nil within 10 s, want the nil that ends its scan", len(keys))
		}
	}
}

// watchKey watches key through kv's jetstream handle and fails the test unless the watch delivers
// the key's value, then the nil that ends its scan.
func watchKey(t *testing.T, kv KeyValue, key string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher, err := kv.WatchKeys(ctx, key)
	if err != nil {
		t.Fatalf("watch %d-byte key: %v", len(key), err)
	}
	defer func() { _ = watcher.Stop() }()
	for _, want := range []bool{true, false} {
		select {
		case entry := <-watcher.Updates():
			if (entry != nil) != want || (entry != nil && entry.Key() != key) {
				t.Fatalf("watch of %d-byte key delivered %v, want the key and then the scan's end", len(key), entry)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("watch of %d-byte key delivered nothing within 10 s", len(key))
		}
	}
}
