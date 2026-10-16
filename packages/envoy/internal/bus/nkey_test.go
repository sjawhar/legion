package bus_test

import (
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nkeys"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// seedFile writes contents to a 0600 file and returns its path.
func seedFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nats.seed")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	return path
}

// unsetNkeyEnvironment clears both seed variables for the test, whatever the shell running it set.
func unsetNkeyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"NATS_NKEY_SEED_FILE", "NATS_NKEY_SEED"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func publishOnce(t *testing.T, client *bus.Client, name string) {
	t.Helper()
	err := client.Publish(contracts.Envelope{
		EventID:        "evt-" + name,
		Source:         "github",
		SourceEventID:  "source-" + name,
		Topic:          "notifications.github.acme.widgets.push.branch.main",
		DedupeKey:      name,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: name,
		TraceID:        "trace-" + name,
	})
	if err != nil {
		t.Fatalf("publish %s: %v", name, err)
	}
}

// With no seed configured, a client connects to a server that asks for no credential, as every
// client did before servers required one. The shared server asks for a password, so the test
// starts one of its own.
func TestAClientWithNoSeedConnectsToAServerWithoutAuthorization(t *testing.T) {
	unsetNkeyEnvironment(t)
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect without a seed: %v", err)
	}
	t.Cleanup(client.Close)
	publishOnce(t, client, "no-seed-open-server")
}

// A server that admits only its nkey users accepts the client that names the user's seed, from
// the file or the variable, and every connection the client dials later to recover is that user's
// too.
func TestAClientWithItsSeedConnectsToAServerThatRequiresIt(t *testing.T) {
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyAuthorized(t, public)

	t.Run("NATS_NKEY_SEED_FILE, which wins over NATS_NKEY_SEED", func(t *testing.T) {
		unsetNkeyEnvironment(t)
		t.Setenv("NATS_NKEY_SEED_FILE", seedFile(t, "  "+seed+"\n"))
		t.Setenv("NATS_NKEY_SEED", "not a seed")
		client, err := bus.ConnectOwningStream([]string{uri})
		if err != nil {
			t.Fatalf("connect with the seed file: %v", err)
		}
		t.Cleanup(client.Close)
		publishOnce(t, client, "seed-file")

		// A closed connection is replaced on the next publish, as the same user.
		client.Conn.Close()
		publishOnce(t, client, "seed-file-after-redial")
	})

	t.Run("NATS_NKEY_SEED", func(t *testing.T) {
		unsetNkeyEnvironment(t)
		t.Setenv("NATS_NKEY_SEED", seed)
		// Connect, which owns no stream, publishes into the stream the first client ensured.
		client, err := bus.Connect([]string{uri})
		if err != nil {
			t.Fatalf("connect with the seed variable: %v", err)
		}
		t.Cleanup(client.Close)
		publishOnce(t, client, "seed-variable")
	})
}

// With no seed configured, the server that requires one refuses the client, and the error says so.
func TestAClientWithNoSeedIsRefusedByAServerThatRequiresOne(t *testing.T) {
	unsetNkeyEnvironment(t)
	_, public := testnats.User(t)
	uri := testnats.StartNkeyAuthorized(t, public)
	client, err := bus.Connect([]string{uri})
	if err == nil {
		client.Close()
		t.Fatal("a client with no seed connected to a server that requires an nkey user")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
		t.Fatalf("refusal = %v, want the server's authorization violation", err)
	}
}

// A seed configured but unusable is an error before any dial, naming the variable and the path,
// never a fallback to NATS_NKEY_SEED or to connecting without a credential. The URL is one nothing
// listens on, so an attempted dial would fail differently.
func TestAnUnusableSeedIsAnErrorNamingItsVariable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.seed")
	blank := seedFile(t, " \n")
	notASeed := seedFile(t, "SUNOTASEED")
	accountSeed := func() string {
		account, err := nkeys.CreateAccount()
		if err != nil {
			t.Fatalf("create nkey account: %v", err)
		}
		raw, err := account.Seed()
		if err != nil {
			t.Fatalf("account seed: %v", err)
		}
		return seedFile(t, string(raw))
	}()
	userSeed, _ := testnats.User(t)
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"a missing file", map[string]string{"NATS_NKEY_SEED_FILE": missing, "NATS_NKEY_SEED": userSeed}, "NATS_NKEY_SEED_FILE names " + missing + ", which could not be read"},
		{"a blank file", map[string]string{"NATS_NKEY_SEED_FILE": blank}, "NATS_NKEY_SEED_FILE names " + blank + ", which is empty"},
		{"an empty pointer", map[string]string{"NATS_NKEY_SEED_FILE": ""}, "NATS_NKEY_SEED_FILE is set but empty"},
		{"a file holding no seed", map[string]string{"NATS_NKEY_SEED_FILE": notASeed}, "NATS_NKEY_SEED_FILE (" + notASeed + ") does not hold a valid nkey seed"},
		{"a file holding an account seed", map[string]string{"NATS_NKEY_SEED_FILE": accountSeed}, "NATS_NKEY_SEED_FILE (" + accountSeed + ") holds an nkey seed that is not a user's"},
		{"a blank variable", map[string]string{"NATS_NKEY_SEED": "  "}, "NATS_NKEY_SEED is set but empty"},
		{"a variable holding no seed", map[string]string{"NATS_NKEY_SEED": "hunter2"}, "NATS_NKEY_SEED does not hold a valid nkey seed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetNkeyEnvironment(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			client, err := bus.Connect([]string{"nats://127.0.0.1:1"})
			if err == nil {
				client.Close()
				t.Fatal("connected with an unusable seed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), userSeed) || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("error %q carries the seed", err)
			}
			if _, err := bus.Dial("nkey-test", []string{"nats://127.0.0.1:1"}, os.LookupEnv); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Dial error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A seed against a server with no users is refused, never silently dropped: the server sends no
// nonce, and nats.go refuses the nkey. This pins the deploy order (a process gets a seed only after
// its server has nkey users) and catches a change that falls back to connecting without it. The
// shared server has users, so the test starts one of its own.
func TestASeedAgainstAServerWithoutUsersIsRefused(t *testing.T) {
	_, uri := testnats.Start(t)
	seed, _ := testnats.User(t)
	unsetNkeyEnvironment(t)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err == nil {
		client.Close()
		t.Fatal("a seed connected to a server with no users")
	}
	if !strings.Contains(err.Error(), "nkeys not supported by the server") {
		t.Fatalf("refusal = %v, want nats.go's nkeys-not-supported error", err)
	}
}

// HoldsSeed finds an nkey seed of any type, in every spelling a reader or a decoder takes for the
// same seed: whole or inside a longer value or base32 run, in either case, percent-encoded up to
// three times, padded, with its last character's unused bits changed (nkeys reads that spelling),
// with '.', '-', '_' or whitespace inside it (nkeys reads line breaks; DNS labels and groups split
// on the others). A value that is no seed is not found, and neither is any clean value of the
// same shapes.
func TestHoldsSeed(t *testing.T) {
	// nats-io's vector: nkeys v0.4.15 README.md line 45, and its public key, line 49.
	const seed = "SUAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43IZY"
	const publicKey = "UD466L6EBCM3YY5HEGHJANNTN4LSKTSUXTH7RILHCKEQMQHTBNLHJJXT"
	if !strings.HasSuffix(seed, "Y") {
		t.Fatal("the spelling row changes a final Y")
	}
	percentEncoded := func(text string) string {
		var b strings.Builder
		for i := range len(text) {
			fmt.Fprintf(&b, "%%%02X", text[i])
		}
		return b.String()
	}
	group := func(sep string, n int) string {
		var parts []string
		for i := 0; i < len(seed); i += n {
			parts = append(parts, seed[i:min(i+n, len(seed))])
		}
		return strings.Join(parts, sep)
	}
	const k = 29 // where a split row cuts the seed
	// encoded is the seed percent-encoded times times, as a URL nested in a URL encodes it again.
	encoded := func(times int) string {
		text := percentEncoded(seed)
		for range times - 1 {
			text = strings.ReplaceAll(text, "%", "%25")
		}
		return text
	}
	// nkeys' decoder reads a seed whose third letter is 'A' to 'D' (it masks the low three bits of
	// the second byte), so a spelling with 'B' there and its checksum recomputed is the same seed,
	// and one with 'E' is none.
	thirdB, thirdE := respelled(t, seed, 'B'), respelled(t, seed, 'E')
	if _, _, err := nkeys.DecodeSeed([]byte(thirdB)); err != nil {
		t.Fatalf("nkeys does not read the third-letter-B spelling: %v", err)
	}
	if _, _, err := nkeys.DecodeSeed([]byte(thirdE)); err == nil {
		t.Fatal("nkeys reads the third-letter-E spelling")
	}
	rows := []struct {
		name string
		text string
		want bool
	}{
		{"the seed", seed, true},
		{"in a file path", "/tmp/example/" + seed, true},
		{"in a URL's path", "https://example.invalid/" + seed + "/x", true},
		{"in a URL's query", "https://example.invalid/?k=" + seed, true},
		{"in a URL's userinfo", "https://" + seed + "@example.invalid/", true},
		{"percent-encoded", "https://example.invalid/" + percentEncoded(seed), true},
		{"partly percent-encoded, beside a percent that encodes nothing", "%zz/" + seed[:10] + percentEncoded(seed[10:]), true},
		{"percent-encoded twice", "https://example.invalid/" + encoded(2), true},
		{"percent-encoded three times", "https://example.invalid/" + encoded(3), true},
		// HoldsSeed decodes at most three times, a documented limit.
		{"percent-encoded four times, past the decoding depth", "https://example.invalid/" + encoded(4), false},
		{"a spelling nkeys reads, its third letter B", thirdB, true},
		{"its third letter E, which nkeys does not read", thirdE, false},
		{"inside a longer run of base32 letters", "AB" + seed + "CD", true},
		{"in an error message", "failed to get shared config profile, " + seed, true},
		{"lowercase", strings.ToLower(seed), true},
		{"mixed case", seed[:20] + strings.ToLower(seed[20:]), true},
		{"lowercase, percent-encoded", "https://example.invalid/" + percentEncoded(strings.ToLower(seed)), true},
		{"padded with = to a multiple of eight", seed + "======", true},
		{"another spelling of its last character, which nkeys reads", strings.TrimSuffix(seed, "Y") + "Z", true},
		{"a line break inside, which nkeys reads", seed[:k] + "\n" + seed[k:], true},
		{"CRLF inside, which nkeys reads", seed[:k] + "\r\n" + seed[k:], true},
		{"wrapped every 16 characters", group("\n", 16), true},
		{"a trailing line break", seed + "\n", true},
		{"grouped by spaces", group(" ", 4), true},
		{"a tab inside", seed[:k] + "\t" + seed[k:], true},
		{"grouped by hyphens", group("-", 4), true},
		{"a proxy host:port, inside a longer base32 label", "AB" + seed + "CD.example.test:1055", true},
		{"a proxy host:port, lowercase", strings.ToLower(seed) + ".example.test:1055", true},
		{"a proxy host:port, percent-encoded", percentEncoded(seed) + ".example.test:1055", true},
		{"a proxy host:port, split across DNS labels", seed[:k] + "." + seed[k:] + ":1055", true},
		{"a proxy host:port, split by a hyphen", seed[:k] + "-" + seed[k:] + ".example.test:1055", true},
		{"a proxy host:port, split by an underscore", seed[:k] + "_" + seed[k:] + ".example.test:1055", true},
		{"a proxy host:port, split across DNS labels, lowercase", strings.ToLower(seed[:k]+"."+seed[k:]) + ":1055", true},
		// The README seed's bytes behind a seed's prefix of the account type, with a valid checksum.
		{"an account seed", "SAAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD443AI", true},
		{"truncated", seed[:57], false},
		{"one character changed", seed[:5] + "A" + seed[6:], false},
		// The same 32 bytes behind the prefix bytes (0, 0), with a valid checksum: no seed.
		{"a seed's bytes behind another prefix byte", "AAAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43I5I", false},
		{"a public key", publicKey, false},
		{"a region", "us-east-1", false},
		{"empty", "", false},
		{"a long base32 run that is no seed", strings.Repeat("SUAKYRHV", 20), false},
		{"a clean proxy host:port split across DNS labels", "abcdefgh.ijklmnop.example.test:1055", false},
		{"a clean proxy host:port split by hyphens and underscores", "socks-proxy_east-1.example.test:1055", false},
		{"a clean sentence", "failed to get shared config profile, example; set AWS_PROFILE", false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := bus.HoldsSeed(row.text); got != row.want {
				t.Fatalf("HoldsSeed = %v, want %v", got, row.want)
			}
		})
	}
	// Seeds of every type nkeys knows, as #1961's check takes them (nkeys.DecodeSeed), 100 of each,
	// so the window check's own letter and checksum tests are shown to keep every seed nkeys reads.
	for name, create := range map[string]func() (nkeys.KeyPair, error){
		"operator": nkeys.CreateOperator, "server": nkeys.CreateServer, "cluster": nkeys.CreateCluster,
		"account": nkeys.CreateAccount, "user": nkeys.CreateUser, "curve": nkeys.CreateCurveKeys,
	} {
		t.Run("generated "+name+" seeds", func(t *testing.T) {
			for range 100 {
				kp, err := create()
				if err != nil {
					t.Fatalf("create %s: %v", name, err)
				}
				generated, err := kp.Seed()
				if err != nil {
					t.Fatalf("%s seed: %v", name, err)
				}
				if !bus.HoldsSeed("value " + string(generated) + " end") {
					t.Fatalf("a generated %s seed is not found", name)
				}
			}
		})
	}
}

// respelled is seed with its third letter set to letter and its checksum recomputed.
func respelled(t *testing.T, seed string, letter byte) string {
	t.Helper()
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding)
	raw, err := b32.DecodeString(seed)
	if err != nil || len(raw) != 36 {
		t.Fatalf("decode %d bytes: %v", len(raw), err)
	}
	// The third letter is the second byte's bits 5 to 1.
	value := byte(strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", letter))
	raw[1] = raw[1]&^0x3e | value<<1
	// CRC-16/XMODEM over all but the last two bytes, which hold it little-endian, as nkeys writes.
	var crc uint16
	for _, b := range raw[:34] {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	raw[34], raw[35] = byte(crc), byte(crc>>8)
	return b32.EncodeToString(raw)
}

// fill is unit repeated to n bytes.
func fill(unit string, n int) string { return strings.Repeat(unit, n/len(unit)+1)[:n] }

// costInputs are 1 MiB values that once cost HoldsSeed the most: a '%' nested 524,288 times, base32
// joined by dashes that starts like a seed, the densest windows that start like one (every third
// letter), those windows again beside nested '%' so every decoding rescans them, and ordinary text.
func costInputs() []struct{ name, text string } {
	const mib = 1 << 20
	return []struct{ name, text string }{
		{"a percent sign nested 524,288 times", "%" + fill("25", mib-1)},
		{"dash-joined base32 that starts like a seed", fill("s-u-", mib)},
		{"a window that starts like a seed every third letter", fill("s-u-a-", mib)},
		{"those windows beside nested percent signs", fill("SUA", mib/2) + "%" + fill("25", mib/2-1)},
		{"ordinary text", fill("the quick brown fox jumps over the lazy dog, so it was said. ", mib)},
	}
}

// HoldsSeed's cost is linear in its input, so each 1 MiB input returns well inside costBound. The
// bound is not a performance target: it separates linear from quadratic. A quadratic cost at 1 MiB
// takes hours (the recursive decoding this replaced took 8.9 s at 32 KiB, and 51 s under the race
// detector, four times as long per doubling). The slowest input here, the windows that every
// decoding rescans, took at most 280 ms, and 1.35 s under the race detector, over three runs on a
// 24-core workstation: 35 and 7 times under the bound. CI runs it without the race detector
// (envoy-and-contracts.yaml's envoy-go job), so a runner slower and busier than that workstation
// still passes it, while a quadratic regression cannot.
func TestHoldsSeedCostIsLinear(t *testing.T) {
	const costBound = 10 * time.Second
	for _, in := range costInputs() {
		t.Run(in.name, func(t *testing.T) {
			done := make(chan bool, 1)
			start := time.Now()
			go func() { done <- bus.HoldsSeed(in.text) }()
			select {
			case found := <-done:
				if found {
					t.Fatal("HoldsSeed found a seed in a value that holds none")
				}
				t.Logf("%d bytes in %v", len(in.text), time.Since(start))
			case <-time.After(costBound):
				t.Fatalf("HoldsSeed did not return within %v on %d bytes", costBound, len(in.text))
			}
		})
	}
}

// HoldsSeed allocates at most one decoded copy per decoding, at most three in all, and nothing for
// the window check, so however deep a value's encoding its memory and its decoding work are
// bounded. Allocations are counted, not timed, so this does not depend on the machine's load. It
// is not parallel: testing.AllocsPerRun counts every malloc in the process. It skips under the race
// detector, as the standard library's allocation tests do (race_test.go says why); its cost
// counterpart, TestHoldsSeedCostIsLinear, still runs there.
func TestHoldsSeedAllocatesAtMostOneCopyPerDecoding(t *testing.T) {
	if raceEnabled {
		t.Skip("skipping in -race mode: the race runtime's own allocations are counted too")
	}
	for _, row := range []struct {
		name string
		text string
		max  float64
	}{
		{"a percent sign nested 1,024 times", "%" + fill("25", 2048), 3},
		{"windows that start like a seed", fill("SUA", 1<<16), 0},
		{"ordinary text", fill("the quick brown fox jumps over the lazy dog. ", 1<<16), 0},
		{"a letter percent-encoded three times", "%252553", 3},
	} {
		t.Run(row.name, func(t *testing.T) {
			if allocs := testing.AllocsPerRun(5, func() { bus.HoldsSeed(row.text) }); allocs > row.max {
				t.Fatalf("HoldsSeed allocated %v times, want at most %v", allocs, row.max)
			}
		})
	}
}
