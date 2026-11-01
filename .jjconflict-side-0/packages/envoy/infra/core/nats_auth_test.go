package core_test

import (
	"encoding/base32"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// header is the header the goldens were rendered with.
var header = []string{"Rendered by envoy-infra from placeholder inputs.", "Public keys only."}

// placeholder is the authorization the goldens were rendered from by the reference implementation.
// Its keys are public keys only; their seeds were discarded when they were made.
func placeholder() core.Authorization {
	return core.Authorization{
		Users: []core.User{
			{
				Name:      "example-listener",
				NKey:      "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
				Publish:   &core.Rules{Allow: []string{"envoy.example.>"}, Deny: []string{"envoy.example.private"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "notifications.café.>"}},
				HasSeed:   true,
			},
			{
				Name:      "example-daemon",
				NKey:      "UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG",
				Publish:   &core.Rules{Allow: []string{"$JS.API.>"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "legion.work workers"}},
				HasSeed:   true,
			},
			{
				Name:      "example-fallback",
				NKey:      "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD",
				Publish:   &core.Rules{Allow: []string{">"}},
				Subscribe: &core.Rules{Allow: []string{">"}},
			},
			{Name: "example-pending", HasSeed: true},
		},
		NoAuthUser: "example-fallback",
	}
}

func golden(t *testing.T, name string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(text)
}

func TestRenderMatchesTheReferenceGolden(t *testing.T) {
	text, err := placeholder().Render(header)
	require.NoError(t, err)
	require.Equal(t, golden(t, "authorization.golden"), text)
}

func TestStartScriptMatchesTheReferenceGolden(t *testing.T) {
	script, err := placeholder().StartScript(header, []string{"-js", "--store_dir", "/data", "-m", "8222"}, "/tmp/nats-server.conf")
	require.NoError(t, err)
	require.Equal(t, golden(t, "start-script.golden"), script)
}

// lenientKeys are three strings nkeys.IsValidPublicUserKey accepts that are not a user public key
// (U and 55 base32 characters with a valid checksum): the base32 decoder skips line breaks, nkeys
// reads a key of any length, and it masks the prefix byte's low three bits. Each is built here with
// a valid checksum, so only the shape refuses it.
func lenientKeys(t *testing.T) map[string]string {
	t.Helper()
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	public, err := user.PublicKey()
	require.NoError(t, err)
	short, err := nkeys.Encode(nkeys.PrefixByteUser, make([]byte, 16))
	require.NoError(t, err)
	masked := append([]byte{byte(nkeys.PrefixByteUser) | 1}, make([]byte, 32)...)
	masked = binary.LittleEndian.AppendUint16(masked, crc16XMODEM(masked))
	keys := map[string]string{
		"a line break inside the key": public[:28] + "\n" + public[28:],
		"a key of another length":     string(short),
		"a prefix byte with low bits": base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(masked),
	}
	for name, key := range keys {
		require.True(t, nkeys.IsValidPublicUserKey(key), "nkeys must accept %s for the row to mean anything", name)
	}
	return keys
}

// crc16XMODEM is the nkeys checksum, written out here so a test can build a key nkeys would make
// no other way. It stays independent of core's own, which is unexported: lenientKeys checks every
// key it builds against nkeys itself, so this copy is held to nkeys rather than to the code under
// test.
func crc16XMODEM(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func TestValidateRefuses(t *testing.T) {
	seed, err := nkeys.CreateUser()
	require.NoError(t, err)
	rawSeed, err := seed.Seed()
	require.NoError(t, err)
	account, err := nkeys.CreateAccount()
	require.NoError(t, err)
	accountKey, err := account.PublicKey()
	require.NoError(t, err)
	listenerKey := placeholder().Users[0].NKey

	type row struct {
		name   string
		change func(a *core.Authorization)
		err    string
	}
	listener := func(a *core.Authorization) *core.User { return &a.Users[0] }
	pending := func(a *core.Authorization) *core.User { return &a.Users[3] }
	nkey := func(key string) func(a *core.Authorization) {
		return func(a *core.Authorization) { listener(a).NKey = key }
	}
	subscribe := func(entry string) func(a *core.Authorization) {
		return func(a *core.Authorization) { listener(a).Subscribe.Allow = []string{"_INBOX.>", entry} }
	}
	rows := []row{
		{"no users", func(a *core.Authorization) { *a = core.Authorization{} }, "users: an authorization needs at least one user"},
		{"every user pending", func(a *core.Authorization) {
			*a = core.Authorization{Users: []core.User{{Name: "example-pending", HasSeed: true}}}
		}, "users: every user is pending"},
		{"an upper-case name", func(a *core.Authorization) { listener(a).Name = "Example-listener" }, `users[0] "Example-listener": name`},
		{"a name starting with a hyphen", func(a *core.Authorization) { listener(a).Name = "-listener" }, `users[0] "-listener": name`},
		{"a name ending with a hyphen", func(a *core.Authorization) { listener(a).Name = "listener-" }, `users[0] "listener-": name`},
		{"an empty name", func(a *core.Authorization) { listener(a).Name = "" }, `users[0] "": name`},
		{"a name of 49 characters", func(a *core.Authorization) { listener(a).Name = strings.Repeat("a", 49) }, `users[0] "` + strings.Repeat("a", 49) + `": name`},
		{"a pending user without a seed", func(a *core.Authorization) { pending(a).HasSeed = false }, `users[3] "example-pending": hasSeed`},
		{"a pending user with a publish rule", func(a *core.Authorization) { pending(a).Publish = &core.Rules{Allow: []string{">"}} }, `users[3] "example-pending": publish`},
		{"a pending user with a subscribe rule", func(a *core.Authorization) { pending(a).Subscribe = &core.Rules{Allow: []string{">"}} }, `users[3] "example-pending": subscribe`},
		{"a keyed user without publish", func(a *core.Authorization) { listener(a).Publish = nil }, `users[0] "example-listener": publish`},
		{"a keyed user without subscribe", func(a *core.Authorization) { listener(a).Subscribe = nil }, `users[0] "example-listener": subscribe`},
		{"a seed for an nkey", nkey(string(rawSeed)), `users[0] "example-listener": nkey`},
		{"an account key", nkey(accountKey), `users[0] "example-listener": nkey`},
		// The listener's key ends in O; the last character carries the checksum's high bits.
		{"a key whose checksum fails", nkey(listenerKey[:55] + "A"), `users[0] "example-listener": nkey`},
		{"a lower-case key", nkey(strings.ToLower(listenerKey)), `users[0] "example-listener": nkey`},
		{"an empty publish allow", func(a *core.Authorization) { listener(a).Publish.Allow = nil }, `users[0] "example-listener": publish.allow`},
		{"an empty subscribe allow", func(a *core.Authorization) { listener(a).Subscribe.Allow = []string{} }, `users[0] "example-listener": subscribe.allow`},
		{"an empty entry", subscribe(""), `users[0] "example-listener": subscribe.allow[1]`},
		{"an empty token", subscribe("a..b"), `users[0] "example-listener": subscribe.allow[1]`},
		{"a leading dot", subscribe(".a"), `subscribe.allow[1]`},
		{"a trailing dot", subscribe("a."), `subscribe.allow[1]`},
		{"a wildcard inside a token", subscribe("a.b*"), `subscribe.allow[1]`},
		{"a full wildcard before the last token", subscribe("a.>.b"), `subscribe.allow[1]`},
		{"three space-separated parts", subscribe("a b c"), `subscribe.allow[1]`},
		{"two spaces between subject and queue", subscribe("a  q"), `subscribe.allow[1]`},
		{"a malformed queue", subscribe("a q*"), `subscribe.allow[1]`},
		{"a tab", subscribe("a\tb"), `subscribe.allow[1]`},
		{"a no-break space", subscribe("a\u00a0b"), `subscribe.allow[1]`},
		{"a line separator", subscribe("a\u2028b"), `subscribe.allow[1]`},
		{"a file separator (U+001C)", subscribe("a\x1cb"), `subscribe.allow[1]`},
		{"DEL", subscribe("a\x7fb"), `subscribe.allow[1]`},
		{"a C1 control", subscribe("a\u0085b"), `subscribe.allow[1]`},
		{"an encoded surrogate", subscribe("a\xed\xa0\x80b"), `subscribe.allow[1]`},
		{"an invalid publish deny entry", func(a *core.Authorization) { listener(a).Publish.Deny = []string{"a..b"} }, `users[0] "example-listener": publish.deny[0]`},
		{"an invalid subscribe deny entry", func(a *core.Authorization) { listener(a).Subscribe.Deny = []string{"a..b"} }, `users[0] "example-listener": subscribe.deny[0]`},
		{"a queue-scoped publish allow", func(a *core.Authorization) { listener(a).Publish.Allow = []string{"envoy.example.>", "a q"} }, `users[0] "example-listener": publish.allow[1]`},
		{"a queue-scoped publish deny", func(a *core.Authorization) { listener(a).Publish.Deny = []string{"a q"} }, `users[0] "example-listener": publish.deny[0]`},
		// nats-server before 2.15 enforces a queue-scoped subscribe deny more loosely than it reads:
		// beside a plain deny it re-allows every other queue group, and alone it misses a wildcard
		// subscription (v2.10.29 server/client.go:3044-3051 and :1199-1203).
		{"a queue deny beside a plain deny", func(a *core.Authorization) { listener(a).Subscribe.Deny = []string{"secret.>", "secret.x q1"} },
			`users[0] "example-listener": subscribe.deny[1] "secret.x q1" names a queue group; nats-server before 2.15 does not enforce a queue-scoped subscribe deny as written`},
		{"a queue deny alone", func(a *core.Authorization) { listener(a).Subscribe.Deny = []string{"secret.x q1"} }, `users[0] "example-listener": subscribe.deny[0]`},
		{"two keyed users with one name", func(a *core.Authorization) { a.Users[1].Name = "example-listener" }, `users[1] "example-listener": name repeats users[0]`},
		{"a pending user with a keyed user's name", func(a *core.Authorization) { pending(a).Name = "example-daemon" }, `users[3] "example-daemon": name repeats users[1]`},
		{"two users with one key", func(a *core.Authorization) { a.Users[1].NKey = listenerKey }, `users[1] "example-daemon": nkey repeats users[0]`},
		{"a fallback naming no user", func(a *core.Authorization) { a.NoAuthUser = "example-nobody" }, `noAuthUser "example-nobody"`},
		{"a pending fallback", func(a *core.Authorization) { a.NoAuthUser = "example-pending" }, `noAuthUser "example-pending"`},
		{"a fallback with a seed", func(a *core.Authorization) { a.Users[2].HasSeed = true }, `noAuthUser "example-fallback"`},
	}
	for name, key := range lenientKeys(t) {
		rows = append(rows, row{"nkeys accepts " + name, nkey(key), `users[0] "example-listener": nkey`})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			a := placeholder()
			r.change(&a)
			require.ErrorContains(t, a.Validate(), r.err)
			_, err := a.Render(header)
			require.ErrorContains(t, err, r.err, "Render renders only what Validate accepts")
			_, err = a.StartScript(header, nil, "/tmp/nats-server.conf")
			requireInputError(t, err, "authorization", r.err)
			require.EqualError(t, err, "authorization: "+a.Validate().Error(), "StartScript renders only what Validate accepts")
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	rows := []struct {
		name   string
		change func(a *core.Authorization)
	}{
		{"the placeholder", func(*core.Authorization) {}},
		{"a one-character name", func(a *core.Authorization) { a.Users[0].Name = "a" }},
		{"a name of 48 characters", func(a *core.Authorization) { a.Users[0].Name = strings.Repeat("a", 48) }},
		{"no fallback, the fallback's user keeping a seed", func(a *core.Authorization) {
			a.NoAuthUser = ""
			a.Users[2].HasSeed = true
		}},
		{"wildcards as whole tokens", func(a *core.Authorization) {
			a.Users[0].Publish.Allow = []string{"*", ">", "a.*.b", "a.>"}
		}},
		{"a wildcard queue", func(a *core.Authorization) {
			a.Users[0].Subscribe.Allow = append(a.Users[0].Subscribe.Allow, "notifications.role.> >")
		}},
		{"a plain subscribe deny", func(a *core.Authorization) { a.Users[0].Subscribe.Deny = []string{"notifications.role.>"} }},
		{"no pending user", func(a *core.Authorization) { a.Users = a.Users[:3] }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			a := placeholder()
			r.change(&a)
			require.NoError(t, a.Validate())
		})
	}
}

// Render refuses a header line holding a line break; StartScript refuses it as configHeader.
func TestRenderRefusesAHeaderLineWithALineBreak(t *testing.T) {
	for _, line := range []string{"one\ntwo", "one\rtwo"} {
		_, err := placeholder().Render([]string{"fine", line})
		require.EqualError(t, err, "header[1] holds a line break; each header line is one comment line")
		_, err = placeholder().StartScript([]string{"fine", line}, nil, "/tmp/nats-server.conf")
		requireInputError(t, err, "configHeader", "header[1]")
		require.EqualError(t, err, "configHeader: header[1] holds a line break; each header line is one comment line")
	}
}

func TestRenderWithoutAFallbackOrAHeader(t *testing.T) {
	a := placeholder()
	a.NoAuthUser = ""
	a.Users = a.Users[2:]
	text, err := a.Render(nil)
	require.NoError(t, err)
	require.Equal(t, "authorization {\n"+
		"  users: [\n"+
		"    # example-fallback\n"+
		`    {"nkey": "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD", "permissions": {"publish": {"allow": [">"]}, "subscribe": {"allow": [">"]}}}`+"\n"+
		"  ]\n"+
		"}\n", text)
}

func TestStartScriptRefusesAnEmptyPath(t *testing.T) {
	_, err := placeholder().StartScript(header, nil, "")
	requireInputError(t, err, "configPath", "configPath")
	require.EqualError(t, err, "configPath is required with authorization: the start script writes the configuration there")
}

// nats-server parses its arguments with Go's flag package, so each of its configuration, credential
// and route flags takes one dash or two and a value after `=` or as the next argument: -c and
// --config load another file in place of the rendered one; -user, -pass and -auth add a
// credential beside its nkey users (nats-server v2.10.29 server/opts.go:5343-5345, :5350-5351);
// and -cluster, -cluster_listen and -routes open or solicit a route, which the rendered block,
// having no cluster authorization, leaves unauthenticated (:5368-5370). StartScript refuses each,
// naming its index and the flag and never the value, which may be a password.
func TestStartScriptRefusesArgumentsThatReplaceTheAuthorization(t *testing.T) {
	rows := []struct{ arg, flag string }{
		{"-c", "c"},
		{"--c", "c"},
		{"-c=/etc/other.conf", "c"},
		{"-config", "config"},
		{"--config", "config"},
		{"--config=/etc/other.conf", "config"},
		{"-auth", "auth"},
		{"--auth=example-token", "auth"},
		{"-user", "user"},
		{"--user=example-user", "user"},
		{"-pass", "pass"},
		{"--pass=example-password", "pass"},
		{"-cluster", "cluster"},
		{"--cluster=nats://0.0.0.0:6222", "cluster"},
		{"-cluster_listen", "cluster_listen"},
		{"--cluster_listen=nats://0.0.0.0:6222", "cluster_listen"},
		{"-routes", "routes"},
		{"--routes=nats://example-host-b.internal.example:6222", "routes"},
	}
	for _, row := range rows {
		t.Run(row.arg, func(t *testing.T) {
			script, err := placeholder().StartScript(header, []string{"-js", row.arg, "value"}, "/tmp/nats-server.conf")
			require.Empty(t, script)
			requireInputError(t, err, "serverArgs", "serverArgs[1] sets nats-server's -"+row.flag+",")
			if _, value, ok := strings.Cut(row.arg, "="); ok {
				require.NotContains(t, err.Error(), value)
			}
		})
	}
}

// A flag whose name only begins like one of those is another flag, and a value is not a flag. A
// cluster name opens no route on its own.
func TestStartScriptAcceptsOtherArguments(t *testing.T) {
	args := []string{"-js", "-cluster_name", "example", "--cluster_advertise", "example", "--users_file", "-m", "8222", "c", "config", "cluster"}
	_, err := placeholder().StartScript(header, args, "/tmp/nats-server.conf")
	require.NoError(t, err)
}

// The start script, run by /bin/sh as a container's `sh -c` runs it, writes exactly Render's text to
// its path and execs nats-server with --config, the path and the server arguments. A stand-in
// nats-server on PATH records the arguments it was given. The path holds a space and a quote, so
// it reaches the shell only through quoting.
func TestStartScriptWritesRenderTextAndExecsTheServer(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o755))
	recorded := filepath.Join(dir, "arguments")
	stand := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done > \"$RECORD\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "nats-server"), []byte(stand), 0o755))
	path := filepath.Join(dir, "nats server's.conf")

	a := placeholder()
	script, err := a.StartScript(header, []string{"-js", "--store_dir", "/data", "-m", "8222"}, path)
	require.NoError(t, err)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "RECORD=" + recorded}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "sh -c: %s", out)

	text, err := a.Render(header)
	require.NoError(t, err)
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, text, string(written))
	arguments, err := os.ReadFile(recorded)
	require.NoError(t, err)
	require.Equal(t, "--config\n"+path+"\n-js\n--store_dir\n/data\n-m\n8222\n", string(arguments))
}
