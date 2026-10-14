package core

import (
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nats-io/nkeys"
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

// userKeyText is a user public key's text: the U of its prefix byte and 55 more base32 characters,
// 35 bytes in all.
var userKeyText = regexp.MustCompile(`^U[A-Z2-7]{55}$`)

var userKeyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Validate refuses an authorization nats-server would read otherwise than its fields say. Every
// refusal names the user, by index and name, and the field.
//
// A pending user (no NKey) stands for a seed the consumer stores before the server knows its key:
// it must have HasSeed, since without one it stands for nothing, and it is never rendered, so rules
// on it are refused. An authorization whose users are all pending, or that has none, is refused too:
// nats-server reads an authorization block with no users as no authentication at all
// (server/auth.go configureAuthorization).
func (a Authorization) Validate() error {
	if len(a.Users) == 0 {
		return errors.New("users: an authorization needs at least one user")
	}
	keyed := 0
	for i, u := range a.Users {
		where := fmt.Sprintf("users[%d] %q", i, u.Name)
		if !userName.MatchString(u.Name) {
			return fmt.Errorf("%s: name must match %s", where, userName)
		}
		if u.NKey == "" {
			if !u.HasSeed {
				return fmt.Errorf("%s: hasSeed is required for a pending user (no nkey): it is never rendered, so without a seed it stands for nothing", where)
			}
			if u.Publish != nil {
				return fmt.Errorf("%s: publish is set on a pending user (no nkey), which is never rendered", where)
			}
			if u.Subscribe != nil {
				return fmt.Errorf("%s: subscribe is set on a pending user (no nkey), which is never rendered", where)
			}
			continue
		}
		keyed++
		if !isUserPublicKey(u.NKey) {
			return fmt.Errorf("%s: nkey is not a user public key (U and 55 base32 characters with a valid checksum); give the public key, never a seed", where)
		}
		if u.Publish == nil {
			return fmt.Errorf("%s: publish is required for a user with an nkey", where)
		}
		if u.Subscribe == nil {
			return fmt.Errorf("%s: subscribe is required for a user with an nkey", where)
		}
		if err := u.Publish.validate("publish", false); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if err := u.Subscribe.validate("subscribe", true); err != nil {
			return fmt.Errorf("%s: %w", where, err)
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
	for _, u := range a.Users {
		if u.Name != a.NoAuthUser {
			continue
		}
		if u.NKey == "" {
			return fmt.Errorf("noAuthUser %q names a pending user (no nkey); a client presenting no credential becomes a user the server knows", a.NoAuthUser)
		}
		if u.HasSeed {
			return fmt.Errorf("noAuthUser %q names a user with a seed (hasSeed); it is the identity of a client presenting no credential, which no seed authenticates", a.NoAuthUser)
		}
		return nil
	}
	return fmt.Errorf("noAuthUser %q names no user", a.NoAuthUser)
}

// isUserPublicKey reports whether key is a user public key: U and 55 base32 characters, whose 35
// bytes are the user prefix byte, the key and a valid CRC-16/XMODEM checksum. nkeys alone
// (IsValidPublicUserKey) reads more than that: its base32 decoder skips line breaks, it takes a key
// of any length, and it masks the prefix byte's low three bits.
func isUserPublicKey(key string) bool {
	if !userKeyText.MatchString(key) || !nkeys.IsValidPublicUserKey(key) {
		return false
	}
	raw, err := userKeyEncoding.DecodeString(key)
	return err == nil && raw[0] == byte(nkeys.PrefixByteUser)
}

// validate refuses rules nats-server would read otherwise than they say: an empty allow list,
// which it never reads as "nothing" (an empty subscribe allow sets no restriction at all,
// server/client.go setPermissions); an entry that is neither a subject pattern nor
// "<subject> <queue>"; and, where queues are not taken, a queue-scoped entry.
func (r *Rules) validate(direction string, queues bool) error {
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
			if queued && !queues {
				return fmt.Errorf(`%s[%d] %q names a queue group, which scopes only subscriptions; a %s rule is a plain subject`, list.field, i, entry, direction)
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
	// nats-server's config lexer ends a comment at \n or \r (conf/lex.go isNL), so a header line
	// holding either would carry the rest of it out of the comment and into the configuration.
	for i, line := range header {
		if strings.ContainsAny(line, "\r\n") {
			return "", fmt.Errorf("header[%d] holds a line break; each header line is one comment line", i)
		}
	}
	var b strings.Builder
	for _, line := range header {
		b.WriteString("# " + line + "\n")
	}
	b.WriteString("authorization {\n  users: [\n")
	fallback := ""
	for _, u := range a.Users {
		if u.NKey == "" {
			continue
		}
		if u.Name == a.NoAuthUser {
			fallback = u.NKey
		}
		user := map[string]any{
			"nkey":        u.NKey,
			"permissions": map[string]any{"publish": u.Publish.object(), "subscribe": u.Subscribe.object()},
		}
		b.WriteString("    # " + u.Name + "\n")
		b.WriteString("    " + PyJSON(user, PyJSONOptions{SortKeys: true}) + "\n")
	}
	b.WriteString("  ]\n}\n")
	if a.NoAuthUser != "" {
		b.WriteString("# " + a.NoAuthUser + ": a client presenting no credential becomes this user\n")
		b.WriteString("no_auth_user: " + PyJSON(fallback, PyJSONOptions{EnsureASCII: true}) + "\n")
	}
	return b.String(), nil
}

// object is the rules as the block writes them: deny only when it names an entry.
func (r *Rules) object() map[string]any {
	object := map[string]any{"allow": r.Allow}
	if len(r.Deny) > 0 {
		object["deny"] = r.Deny
	}
	return object
}

// StartScript returns the `sh -c` script that writes Render's text to path and execs nats-server
// with --config path and serverArgs.
func (a Authorization) StartScript(header []string, serverArgs []string, path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required: the start script writes the configuration there")
	}
	text, err := a.Render(header)
	if err != nil {
		return "", err
	}
	command := append([]string{"nats-server", "--config", path}, serverArgs...)
	return "set -eu; printf '%s' " + ShellQuote(text) + " > " + ShellQuote(path) + "; exec " + ShellJoin(command), nil
}
