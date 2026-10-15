package core

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/bus"
)

// User is one nkey user of a NATS server's authorization block.
type User struct {
	Name      string // ^[a-z0-9](?:[a-z0-9-]{0,46}[a-z0-9])?$
	NKey      string // nkey user public key; "" for a pending user, which is never rendered
	Publish   *Rules // required with NKey, refused without
	Subscribe *Rules // required with NKey, refused without
	HasSeed   bool   // the consumer stores a seed for this user
}

// Rules are one direction's permissions. Each entry is a subject pattern; a subscribe entry may
// instead be "<subject> <queue>", which scopes it to that queue group.
type Rules struct {
	Allow []string // must name an entry: nats-server reads an empty subscribe allow as "everything"
	Deny  []string
}

// Authorization is a NATS server's static nkey authorization.
type Authorization struct {
	Users      []User
	NoAuthUser string // "" for none; else the Name of a keyed user without a seed
}

var userName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,46}[a-z0-9])?$`)

// Validate refuses an authorization nats-server would read otherwise than its fields say. Every
// refusal names the user, by index and name, and the field.
//
// First it refuses a user's name, a rule entry (allow or deny, publish or subscribe, a queue group
// included) or noAuthUser that holds an nkey seed (bus.HoldsSeed), naming its path and never the
// value, since a refusal quotes each and the rendered block writes them (seedField). The nkey
// field needs no such check: one that is not a user public key is refused without being quoted.
//
// A pending user (no NKey) stands for a seed the consumer stores before the server knows its key:
// it must have HasSeed, since without one it stands for nothing, and it is never rendered, so rules
// on it are refused. An authorization whose users are all pending, or that has none, is refused too:
// nats-server reads an authorization block with no users as no authentication at all
// (server/auth.go configureAuthorization).
func (a Authorization) Validate() error {
	if field := a.seedField(); field != "" {
		return SeedRefusal(field)
	}
	if len(a.Users) == 0 {
		return errors.New("users: an authorization needs at least one user")
	}
	keyed := 0
	for i, u := range a.Users {
		if err := u.validate(); err != nil {
			return fmt.Errorf("users[%d] %q: %w", i, u.Name, err)
		}
		if u.NKey != "" {
			keyed++
		}
	}
	if keyed == 0 {
		return errors.New("users: every user is pending (no nkey), which renders a block naming no users, and nats-server reads that as no authentication at all")
	}
	for i, u := range a.Users {
		for j := range i {
			if a.Users[j].Name == u.Name {
				return fmt.Errorf("users[%d] %q: name repeats users[%d]", i, u.Name, j)
			}
			if u.NKey != "" && a.Users[j].NKey == u.NKey {
				return fmt.Errorf("users[%d] %q: nkey repeats users[%d] %q", i, u.Name, j, a.Users[j].Name)
			}
		}
	}
	if a.NoAuthUser == "" {
		return nil
	}
	switch u, ok := a.user(a.NoAuthUser); {
	case !ok:
		return fmt.Errorf("noAuthUser %q names no user", a.NoAuthUser)
	case u.NKey == "":
		return fmt.Errorf("noAuthUser %q names a pending user (no nkey); a client presenting no credential becomes a user the server knows", a.NoAuthUser)
	case u.HasSeed:
		return fmt.Errorf("noAuthUser %q names a user with a seed (hasSeed); it is the identity of a client presenting no credential, which no seed authenticates", a.NoAuthUser)
	}
	return nil
}

// user returns the user named name: the first, since Validate refuses a name given twice.
func (a Authorization) user(name string) (User, bool) {
	for _, u := range a.Users {
		if u.Name == name {
			return u, true
		}
	}
	return User{}, false
}

// seedField names, by its path, the first name, rule entry or noAuthUser that holds an nkey user
// seed, or returns "". Every name is checked before any entry, so an entry's path quotes its user's
// name only once that name is known to hold none.
func (a Authorization) seedField() string {
	for i, u := range a.Users {
		if bus.HoldsSeed(u.Name) {
			return fmt.Sprintf("users[%d]: name", i)
		}
	}
	for i, u := range a.Users {
		for _, d := range []struct {
			field string
			rules *Rules
		}{{"publish", u.Publish}, {"subscribe", u.Subscribe}} {
			if d.rules == nil {
				continue
			}
			for _, list := range []struct {
				field   string
				entries []string
			}{{d.field + ".allow", d.rules.Allow}, {d.field + ".deny", d.rules.Deny}} {
				if j := slices.IndexFunc(list.entries, bus.HoldsSeed); j >= 0 {
					return fmt.Sprintf("users[%d] %q: %s[%d]", i, u.Name, list.field, j)
				}
			}
		}
	}
	if bus.HoldsSeed(a.NoAuthUser) {
		return "noAuthUser"
	}
	return ""
}

// validate refuses one user, naming the field: a name the pattern refuses; for a pending user, no
// seed or a rule set; for a keyed user, a key that is not a user public key, a direction without
// rules, or rules nats-server would read otherwise than they say.
func (u User) validate() error {
	if !userName.MatchString(u.Name) {
		return fmt.Errorf("name must match %s", userName)
	}
	directions := []struct {
		field string
		rules *Rules
	}{{"publish", u.Publish}, {"subscribe", u.Subscribe}}
	if u.NKey == "" {
		if !u.HasSeed {
			return errors.New("hasSeed is required for a pending user (no nkey): it is never rendered, so without a seed it stands for nothing")
		}
		for _, d := range directions {
			if d.rules != nil {
				return fmt.Errorf("%s is set on a pending user (no nkey), which is never rendered", d.field)
			}
		}
		return nil
	}
	if !IsUserPublicKey(u.NKey) {
		return errors.New("nkey is not a user public key (U and 55 base32 characters with a valid checksum); give the public key, never a seed")
	}
	for _, d := range directions {
		if d.rules == nil {
			return fmt.Errorf("%s is required for a user with an nkey", d.field)
		}
	}
	for _, d := range directions {
		if err := d.rules.validate(d.field); err != nil {
			return err
		}
	}
	return nil
}

// validate refuses rules nats-server would read otherwise than they say: an empty allow list,
// which it never reads as "nothing" (an empty subscribe allow sets no restriction at all,
// server/client.go setPermissions); an entry that is neither a subject pattern nor
// "<subject> <queue>"; a queue-scoped entry outside subscribe, the one direction queue groups
// scope; and a queue-scoped subscribe deny, which nats-server before 2.15 enforces more loosely
// than it reads. Beside a plain deny it re-allows every other queue group, and alone it misses a
// wildcard subscription that covers its subject (v2.10.29 server/client.go:3044-3051, the deny
// filter at :1199-1203; v2.15.1 fixes both at :3454 and :1398-1402).
func (r *Rules) validate(direction string) error {
	if len(r.Allow) == 0 {
		return fmt.Errorf(`%s.allow must name at least one entry; write [">"] to mean everything`, direction)
	}
	for _, list := range []struct {
		field   string
		entries []string
	}{{direction + ".allow", r.Allow}, {direction + ".deny", r.Deny}} {
		for i, entry := range list.entries {
			subject, queue, queued := strings.Cut(entry, " ")
			if !isSubjectPattern(subject) || (queued && !isSubjectPattern(queue)) {
				return fmt.Errorf(`%s[%d] %q is neither a subject pattern nor "<subject> <queue>"`, list.field, i, entry)
			}
			if queued && direction != "subscribe" {
				return fmt.Errorf(`%s[%d] %q names a queue group, which scopes only subscriptions; a %s rule is a plain subject`, list.field, i, entry, direction)
			}
			if queued && list.field == "subscribe.deny" {
				return fmt.Errorf(`%s[%d] %q names a queue group; nats-server before 2.15 does not enforce a queue-scoped subscribe deny as written: write a plain subject, or narrow subscribe.allow`, list.field, i, entry)
			}
		}
	}
	return nil
}

// isSubjectPattern reports whether s is a NATS subject or wildcard pattern: dot-separated non-empty
// tokens, * and > only as whole tokens, > only last, and no whitespace (unicode.IsSpace), control
// character (Cc) or surrogate. Whitespace matters beyond appearance: nats-server splits a
// permission entry on every Unicode space (server/opts.go checkPermSubjectArray, strings.Fields),
// so "a\u00a0q" would be enforced as subject a in queue group q. A surrogate has no UTF-8
// encoding, so Go reads one as invalid UTF-8.
func isSubjectPattern(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	tokens := strings.Split(s, ".")
	for i, token := range tokens {
		if token == "" || strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return false
		}
		if strings.ContainsAny(token, "*>") && token != "*" && token != ">" {
			return false
		}
		if token == ">" && i != len(tokens)-1 {
			return false
		}
	}
	return true
}

// Render returns the authorization block nats-server reads: the header lines, each as
// "# <line>", then the block. Each keyed user is a comment naming it and one JSON object (keys
// sorted, non-ASCII raw: nats-server's config strings take no \u escape); a pending user is not
// rendered. no_auth_user is a top-level option, so it follows the block, naming the fallback
// user's key. The text ends with one newline.
func (a Authorization) Render(header []string) (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	if err := checkHeader(header); err != nil {
		return "", err
	}
	return a.render(header), nil
}

// checkHeader refuses a header line holding an nkey seed (bus.HoldsSeed), which the rendered text
// would carry, and a line holding a line break. nats-server's config lexer ends a comment at \n or
// \r (conf/lex.go isNL), so a line holding either would carry the rest of it out of the comment and
// into the configuration.
func checkHeader(header []string) error {
	for i, line := range header {
		if bus.HoldsSeed(line) {
			return SeedRefusal(fmt.Sprintf("header[%d]", i))
		}
		if strings.ContainsAny(line, "\r\n") {
			return fmt.Errorf("header[%d] holds a line break; each header line is one comment line", i)
		}
	}
	return nil
}

// render is Render's text, for an authorization Validate accepts and a header checkHeader accepts.
func (a Authorization) render(header []string) string {
	var b strings.Builder
	for _, line := range header {
		b.WriteString("# " + line + "\n")
	}
	b.WriteString("authorization {\n  users: [\n")
	for _, u := range a.Users {
		if u.NKey == "" {
			continue
		}
		user := Object{
			{"nkey", u.NKey},
			{"permissions", Object{{"publish", u.Publish.object()}, {"subscribe", u.Subscribe.object()}}},
		}
		b.WriteString("    # " + u.Name + "\n")
		b.WriteString("    " + PyJSON(user, PyJSONOptions{SortKeys: true}) + "\n")
	}
	b.WriteString("  ]\n}\n")
	if a.NoAuthUser != "" {
		fallback, _ := a.user(a.NoAuthUser) // Validate has checked it names a keyed user
		b.WriteString("# " + a.NoAuthUser + ": a client presenting no credential becomes this user\n")
		b.WriteString("no_auth_user: " + PyJSON(fallback.NKey, PyJSONOptions{EnsureASCII: true}) + "\n")
	}
	return b.String()
}

// object is the rules as the block writes them: deny only when it names an entry.
func (r *Rules) object() Object {
	object := Object{{"allow", r.Allow}}
	if len(r.Deny) > 0 {
		object = append(object, Field{"deny", r.Deny})
	}
	return object
}

// StartScript returns the `sh -c` script that writes Render's text to configPath and execs
// nats-server with --config configPath and serverArgs. It refuses a config path or a server
// argument that holds an nkey seed (bus.HoldsSeed), which the script would carry, naming it and
// never the value, and a server argument that would set aside the rendered authorization
// (authorizationFlags). Each refusal is an InputError naming its input as a component's schema
// does: authorization (Validate's refusal, run once here), configPath, serverArgs and
// configHeader, checked in that order, so a caller passes a refusal on as it is.
func (a Authorization) StartScript(configHeader, serverArgs []string, configPath string) (string, error) {
	if err := a.Validate(); err != nil {
		return "", InputErrorf("authorization", "authorization: %w", err)
	}
	if configPath == "" {
		return "", InputErrorf("configPath", "configPath is required with authorization: the start script writes the configuration there")
	}
	if bus.HoldsSeed(configPath) {
		return "", InputErrorf("configPath", "%w", SeedRefusal("configPath"))
	}
	for i, arg := range serverArgs {
		if bus.HoldsSeed(arg) {
			return "", InputErrorf("serverArgs", "%w", SeedRefusal(fmt.Sprintf("serverArgs[%d]", i)))
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if why, ok := authorizationFlags[name]; ok && strings.HasPrefix(arg, "-") {
			// The value is not quoted: after -pass or -auth it is a credential.
			return "", InputErrorf("serverArgs", "serverArgs[%d] sets nats-server's -%s, which %s", i, name, why)
		}
	}
	if err := checkHeader(configHeader); err != nil {
		return "", InputErrorf("configHeader", "configHeader: %w", err)
	}
	command := append([]string{"nats-server", "--config", configPath}, serverArgs...)
	return "set -eu; printf '%s' " + ShellQuote(a.render(configHeader)) + " > " + ShellQuote(configPath) + "; exec " + ShellJoin(command), nil
}

// authorizationFlags are the nats-server flags a server argument must not set, with what each does
// to the rendered authorization. nats-server parses its arguments with Go's flag package, so each
// takes one dash or two (nats-server v2.10.29 server/opts.go:5343-5345, :5350-5351, :5368-5370).
// The rendered block has no cluster authorization, so a route opened or solicited by a flag is
// unauthenticated, and a server routed to it sees every message its users publish.
var authorizationFlags = map[string]string{
	"c":              "loads another configuration file in place of the rendered one",
	"config":         "loads another configuration file in place of the rendered one",
	"auth":           "adds a token beside the rendered nkey users",
	"user":           "adds a user beside the rendered nkey users",
	"pass":           "sets a password beside the rendered nkey users",
	"cluster":        "opens or solicits a route that no client permission binds",
	"cluster_listen": "opens or solicits a route that no client permission binds",
	"routes":         "opens or solicits a route that no client permission binds",
}
