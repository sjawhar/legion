// packages/envoy/internal/broker/requests/airlock.go
// airlock.go is the placeholder side of the airlock: the strings a release hands a session in
// place of a secret the rules file names, the table those strings live in
// (airlock_placeholders, migration 0028), the shape each kind of secret is released in once its
// values are taken out of it, and the two reads the broker's proxy port makes — one per
// connection (AirlockConnection) and one per grant per pod (AirlockValues). The proxy itself,
// which decides and rewrites each request, is internal/broker/airlock.
package requests

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
)

// ErrAirlockNoNames is AirlockConnection's refusal of a placeholder whose grant is live and holds
// no airlock name the session may still have: every one of them is withheld from it. The proxy
// answers it 407, as it answers an unknown placeholder, since a connection that may carry nothing
// is no connection.
var ErrAirlockNoNames = errors.New("this grant holds no airlock name the session may still have")

// The slots of a secret. A slot names which value of a secret a placeholder stands for, because
// one secret can release several values at once and each needs a placeholder of its own:
//
//   - SlotValue: a stored secret's own value, and a google-login's access token;
//   - the three aws- slots: a stored AWS key pair, which the secret's airlock entry marks with
//     kind "aws"; the session token's slot exists only when the stored value holds one;
//   - SlotGitHubPrefix + owner: one installation of a github-app secret, the owner lower-cased;
//   - SlotAzureFederated: an azure-login secret's federated token.
//
// The set is open, not closed: the proxy adds azure-access:<scope> when it exchanges the
// federated token, so nothing may assume a slot it does not know is a mistake.
const (
	SlotValue              = "value"
	SlotAWSAccessKeyID     = "aws-access-key-id"
	SlotAWSSecretAccessKey = "aws-secret-access-key"
	SlotAWSSessionToken    = "aws-session-token"
	SlotGitHubPrefix       = "github:"
	SlotAzureFederated     = "azure-federated"
)

// airlockKindAWS is the airlock entry kind of a stored AWS key pair, the one entry kind that
// changes which slots a stored secret fills (airlock.SecretRule.Kind).
const airlockKindAWS = "aws"

// PlaceholderPrefix opens every placeholder, so a value that leaks into a log or a bug report is
// recognizable for what it is: a string that stands for a secret rather than a secret.
const PlaceholderPrefix = "agent-secrets-placeholder-"

// placeholderAlphabet and placeholderLength are what follows the prefix: 32 characters of 62, so
// about 190 bits, which nobody guesses and no birthday collision reaches.
const (
	placeholderAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	placeholderLength   = 32
)

// mintPlaceholder draws one placeholder from crypto/rand.
//
// Each character takes its own uniform 64-bit draw reduced modulo 62. 62 does not divide 2^64, so
// the first 2^64 mod 62 characters of the alphabet are favoured by about one part in 2^58 — a
// difference nothing can observe — where the obvious byte-per-character reduction would favour
// the first eight characters by one part in four. Reducing a wide draw rejects nothing and so
// always costs one read of the random source, where redrawing a byte until it falls below 248
// loops an unbounded number of times.
func mintPlaceholder() (string, error) {
	draws := make([]byte, 8*placeholderLength)
	if _, err := rand.Read(draws); err != nil {
		return "", fmt.Errorf("mint an airlock placeholder: %w", err)
	}
	out := make([]byte, 0, len(PlaceholderPrefix)+placeholderLength)
	out = append(out, PlaceholderPrefix...)
	for i := range placeholderLength {
		out = append(out, placeholderAlphabet[binary.BigEndian.Uint64(draws[8*i:])%uint64(len(placeholderAlphabet))])
	}
	return string(out), nil
}

// placeholderHash is a placeholder's SHA-256 in hex: the key airlock_placeholders holds it under,
// and the only form of it any query carries, so the plaintext a command holds never reaches a
// statement log, pg_stat_statements or a database error.
func placeholderHash(placeholder string) string {
	sum := sha256.Sum256([]byte(placeholder))
	return hex.EncodeToString(sum[:])
}

// placeholdersOf reads every placeholder grant grantID holds, keyed by name and then by slot. It
// is one indexed read: grant_id leads the unique constraint's index.
func placeholdersOf(ctx context.Context, q querier, grantID string) (map[string]map[string]string, error) {
	rows, err := q.Query(ctx, `select name, slot, placeholder from airlock_placeholders where grant_id=$1`, grantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := map[string]map[string]string{}
	for rows.Next() {
		var name, slot, placeholder string
		if err := rows.Scan(&name, &slot, &placeholder); err != nil {
			return nil, err
		}
		if held[name] == nil {
			held[name] = map[string]string{}
		}
		held[name][slot] = placeholder
	}
	return held, rows.Err()
}

// mintPlaceholders answers the placeholder of every (name, slot) pair want names for grantID: the
// one the grant already holds, or a freshly minted one written with the grant's own expiry. A
// second release of one grant therefore hands back the placeholders the first one did and writes
// no row, which is what the unique (grant_id, name, slot) constraint makes true even for two
// releases racing on different pods — the loser's insert does nothing and the rows are read again
// rather than assumed. A pair the grant holds a row for that want no longer names (a key pair
// that lost its session token) keeps its row: it names no value the proxy can serve, and deleting
// it would cost a write on every release to save a row the grant's end removes anyway.
func (m *Machine) mintPlaceholders(ctx context.Context, grantID string, expires *time.Time, want map[string][]string) (map[string]map[string]string, error) {
	held, err := placeholdersOf(ctx, m.Store.Pool, grantID)
	if err != nil {
		return nil, err
	}
	var names, slots, texts, hashes []string
	for _, name := range slices.Sorted(maps.Keys(want)) {
		for _, slot := range want[name] {
			if _, minted := held[name][slot]; minted {
				continue
			}
			placeholder, err := mintPlaceholder()
			if err != nil {
				return nil, err
			}
			names, slots = append(names, name), append(slots, slot)
			texts, hashes = append(texts, placeholder), append(hashes, placeholderHash(placeholder))
		}
	}
	if len(names) == 0 {
		return held, nil
	}
	if _, err := m.Store.Pool.Exec(ctx, `insert into airlock_placeholders (placeholder_sha256, placeholder, grant_id, name, slot, expires_at)
		select hash, placeholder, $1, name, slot, $6 from unnest($2::text[], $3::text[], $4::text[], $5::text[]) as minted(name, slot, placeholder, hash)
		on conflict (grant_id, name, slot) do nothing`, grantID, names, slots, texts, hashes, expires); err != nil {
		return nil, err
	}
	return placeholdersOf(ctx, m.Store.Pool, grantID)
}

// deleteSpentPlaceholders deletes the placeholders of every grant that is revoked or has expired,
// answering how many rows went. The sweeper calls it each tick; see its comment for why a grant's
// rows may go the moment the grant ends.
func (m *Machine) deleteSpentPlaceholders(ctx context.Context) (int64, error) {
	tag, err := m.Store.Pool.Exec(ctx, `delete from airlock_placeholders p using grants g
		where g.id = p.grant_id and (g.revoked_at is not null or (g.expires_at is not null and g.expires_at <= now()))`)
	return tag.RowsAffected(), err
}

// AirlockRelease is how a release tells a session to reach the values it did not hand over: the
// proxy that holds them, the certificate authority to trust it by, the password every connection
// to it opens with, and each name the airlock holds back.
type AirlockRelease struct {
	ProxyURL      string
	CACertificate string // PEM
	ProxyPassword string
	Names         map[string]AirlockName
}

// AirlockName is one name the airlock holds back: what the command's environment variable of that
// name holds, the placeholder of each slot that value is built from, and the hosts the entry
// allows, which the command keeps off its NO_PROXY so they reach the proxy.
type AirlockName struct {
	Value string            // what the command's NAME holds
	Slots map[string]string // slot -> placeholder
	Hosts []string          // the entry's hosts, which the command keeps off its NO_PROXY
}

// airlockShape is one airlock name's released value with the real value already taken out of it:
// the slots the value fills, and how to put each slot's placeholder where its own value was.
type airlockShape struct {
	// slots are the name's slots, sorted, which is the order the proxy password is chosen in.
	slots []string
	// fill answers what the session holds for this name, taking each slot's placeholder from
	// bySlot.
	fill func(bySlot map[string]string) (string, error)
}

// airlockShapeOf reads value, the real value a release of an airlock name read or minted, and
// answers the shape the session gets in its place. The shape is today's released shape with
// placeholders where the secret was, so a consumer that works today works unchanged:
//
//   - a stored secret and a google-login access token are one string, so they are the value
//     slot's placeholder alone;
//   - a stored AWS key pair, which the entry marks kind "aws", is the stored object re-emitted
//     with each of its three credential fields replaced;
//   - a github-app's minted installations and an azure-login's minted identity are re-emitted
//     with the token fields replaced and every other field as the minter wrote it.
//
// The policy kind decides first and the entry's kind only for a stored secret, because the kind
// is what says how the broker built the value, where the entry says where that value may go. A
// value that does not hold what its kind releases — no object, a missing or non-string token — is
// an error rather than a release, since the one other answer would be to release the real value
// the rules hold back.
func airlockShapeOf(entry *airlock.SecretRule, kind, value string) (airlockShape, error) {
	switch {
	case kind == policy.KindGitHubApp:
		return githubAppShape(value)
	case kind == policy.KindAzureLogin:
		return objectShape(value, map[string]string{"federated_token": SlotAzureFederated}, nil)
	case kind == "" && entry.Kind == airlockKindAWS:
		return objectShape(value, map[string]string{"access_key_id": SlotAWSAccessKeyID, "secret_access_key": SlotAWSSecretAccessKey},
			map[string]string{"session_token": SlotAWSSessionToken})
	default:
		return valueShape(), nil
	}
}

// valueShape is the shape of a value that is one string: the value slot's placeholder, alone.
func valueShape() airlockShape {
	return airlockShape{slots: []string{SlotValue}, fill: func(bySlot map[string]string) (string, error) { return bySlot[SlotValue], nil }}
}

// objectShape is the shape of a JSON object whose fields hold the secrets: required names every
// field that must be a string, optional every field that is one when it is there, each against
// the slot that stands for it. Every other field of the object is released as it was.
func objectShape(value string, required, optional map[string]string) (airlockShape, error) {
	doc, err := jsonObject(value)
	if err != nil {
		return airlockShape{}, err
	}
	holes := map[string]string{} // field -> slot
	for field, slot := range required {
		if _, ok := jsonString(doc[field]); !ok {
			return airlockShape{}, fmt.Errorf("its value holds no %s string, which the airlock stands a placeholder in for", field)
		}
		holes[field] = slot
	}
	for field, slot := range optional {
		if _, ok := jsonString(doc[field]); ok {
			holes[field] = slot
		}
	}
	return airlockShape{slots: sortedValues(holes), fill: func(bySlot map[string]string) (string, error) {
		filled := maps.Clone(doc)
		for field, slot := range holes {
			filled[field] = jsonStringOf(bySlot[slot])
		}
		return marshalObject(filled)
	}}, nil
}

// githubAppShape is the shape of a github-app secret's minted installations: each installation's
// token replaced by the placeholder of its owner's slot, every other field of the document and of
// each installation as the minter wrote it. The slot lower-cases the owner, where the document
// keeps the key GitHub answered with.
func githubAppShape(value string) (airlockShape, error) {
	doc, err := jsonObject(value)
	if err != nil {
		return airlockShape{}, err
	}
	var installations map[string]map[string]json.RawMessage
	if json.Unmarshal(doc["installations"], &installations) != nil {
		return airlockShape{}, errors.New("its minted value holds no installations object, which the airlock stands a placeholder in for")
	}
	slotOf := map[string]string{} // installation key -> slot
	for owner, installation := range installations {
		if _, ok := jsonString(installation["token"]); !ok {
			return airlockShape{}, fmt.Errorf("its minted installation %q holds no token string, which the airlock stands a placeholder in for", owner)
		}
		slot := SlotGitHubPrefix + strings.ToLower(owner)
		if other, clash := slotOf[slot]; clash {
			return airlockShape{}, fmt.Errorf("its minted installations %q and %q are the same owner, so one placeholder would stand for both", other, owner)
		}
		slotOf[slot] = owner
	}
	return airlockShape{slots: sortedKeys(slotOf), fill: func(bySlot map[string]string) (string, error) {
		filled := maps.Clone(installations)
		for slot, owner := range slotOf {
			installation := maps.Clone(filled[owner])
			installation["token"] = jsonStringOf(bySlot[slot])
			filled[owner] = installation
		}
		held, err := json.Marshal(filled)
		if err != nil {
			return "", err
		}
		out := maps.Clone(doc)
		out["installations"] = held
		return marshalObject(out)
	}}, nil
}

// jsonObject reads a value the airlock must take apart as a JSON object, keeping each field's own
// bytes so every field it does not replace is released exactly as it was read or minted.
func jsonObject(value string) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &doc) != nil || doc == nil {
		return nil, errors.New("its value is not a JSON object, which the airlock must take apart to stand placeholders in for its fields")
	}
	return doc, nil
}

// jsonString reads a field that must be a JSON string, answering false for a field that is
// missing or is anything else.
func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// jsonStringOf is a placeholder as one JSON field. A placeholder is 58 characters of the JSON
// string alphabet, so this never fails and the error is dropped.
func jsonStringOf(placeholder string) json.RawMessage {
	raw, _ := json.Marshal(placeholder)
	return raw
}

// marshalObject re-emits a document whose fields are raw JSON. The fields come out sorted by
// name, since Go marshals a map that way; a JSON object's field order carries no meaning.
func marshalObject(doc map[string]json.RawMessage) (string, error) {
	out, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sortedKeys and sortedValues are a map's keys and values, sorted, so a shape's slots are in one
// order whatever the map iteration gave.
func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }

func sortedValues(m map[string]string) []string { return slices.Sorted(maps.Values(m)) }

// airlockRelease mints each airlock name's placeholders, writes proxy as those names' delivery,
// and answers what the session is told: where the proxy is, the certificate to trust it by, the
// password its connections open with, and each name's released value, slots and hosts.
func (m *Machine) airlockRelease(ctx context.Context, grantID string, expires *time.Time, shapes map[string]airlockShape) (*AirlockRelease, error) {
	want := map[string][]string{}
	for name, shape := range shapes {
		want[name] = shape.slots
	}
	minted, err := m.mintPlaceholders(ctx, grantID, expires, want)
	if err != nil {
		return nil, err
	}
	released := &AirlockRelease{ProxyURL: m.AirlockProxyURL, CACertificate: m.AirlockCACertificate, Names: map[string]AirlockName{}}
	for name, shape := range shapes {
		slots := map[string]string{}
		for _, slot := range shape.slots {
			slots[slot] = minted[name][slot]
		}
		value, err := shape.fill(slots)
		if err != nil {
			return nil, fmt.Errorf("release %s behind the airlock: %w", name, err)
		}
		released.Names[name] = AirlockName{Value: value, Slots: slots, Hosts: m.Airlock.Lookup(name).Hosts}
	}
	released.ProxyPassword = proxyPassword(released.Names)
	// Which names went out as a placeholder rather than as their value, recorded where the
	// request's own decision for each name is. Nothing reads it; it is the audit trail's answer
	// to "what did this grant actually hand over".
	if _, err := m.Store.Pool.Exec(ctx, `update request_secrets set delivery='proxy'
		where request_id = (select request_id from grants where id=$1) and name = any($2)`,
		grantID, slices.Sorted(maps.Keys(shapes))); err != nil {
		return nil, err
	}
	return released, nil
}

// proxyPassword is the placeholder of the first (name, slot) pair in sorted order, by name and
// then by slot. Every placeholder of a live grant opens a connection anyway — they sit in one
// command's environment and are of equal power — so one of them has to be the password, and
// taking the first in sorted order is what makes a grant of several names unambiguous for the
// command and for the proxy.
func proxyPassword(names map[string]AirlockName) string {
	for _, name := range slices.Sorted(maps.Keys(names)) {
		slots := names[name].Slots
		for _, slot := range slices.Sorted(maps.Keys(slots)) {
			return slots[slot]
		}
	}
	return ""
}

// AirlockConnection is what one proxy connection's password proves: the live grant behind it and
// every airlock name it may carry. It runs the checks that must hold on EVERY connection - the
// grant's liveness, its enrollment's lease, and the names its operator withheld - and no others;
// the full release checks run once per grant per pod, in AirlockValues.
type AirlockConnection struct {
	GrantID, EnrollmentID, RequestID string
	Issue                            string // the first Dispatch issue key in the request's own signed reason, "" when it names none
	Expires                          *time.Time
	Names                            map[string]AirlockGrantName
}

// AirlockGrantName is one name a connection may carry: the placeholder of each of its slots, and
// the kind that says what those slots are.
type AirlockGrantName struct {
	Slots map[string]string // slot -> placeholder
	Kind  string            // the policy kind: "", "github-app", "google-login", "azure-login"
}

// AirlockConnection answers what the placeholder a proxy connection authenticated with proves. It
// refuses with ErrGrantNotLive when no row holds that placeholder, when its grant is revoked or
// expired, or when its enrollment is revoked or its lease lapsed; it drops every name the
// enrollment's operator withheld, and refuses with ErrAirlockNoNames when that leaves none, which
// the proxy answers 407.
//
// This runs on every connection, so it is indexed reads alone and nothing else: the row by its
// primary key with its grant and enrollment joined in, the grant's other placeholders by
// grant_id, and the enrollment's withheld names by enrollment_id. Each name's kind comes from the
// policy snapshot already in memory, which costs no query. The checks a release makes besides —
// the approval chain, the policy re-evaluation, the max-grant and the airlock fingerprint — are
// AirlockValues's, once per grant per pod.
func (m *Machine) AirlockConnection(ctx context.Context, placeholder string) (AirlockConnection, error) {
	var conn AirlockConnection
	var reason string
	var live bool
	err := m.Store.Pool.QueryRow(ctx, `select p.grant_id, g.enrollment_id, g.request_id, g.expires_at, r.reason,
		g.revoked_at is null and `+grantUnexpired("g")+` and e.revoked_at is null and enrollment_lease_live(e.lease_expires_at)
		from airlock_placeholders p
		join grants g on g.id = p.grant_id
		join enrollments e on e.id = g.enrollment_id
		join requests r on r.id = g.request_id
		where p.placeholder_sha256 = $1`, placeholderHash(placeholder)).
		Scan(&conn.GrantID, &conn.EnrollmentID, &conn.RequestID, &conn.Expires, &reason, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return AirlockConnection{}, ErrGrantNotLive
	}
	if err != nil {
		return AirlockConnection{}, err
	}
	if !live {
		return AirlockConnection{}, ErrGrantNotLive
	}
	conn.Issue = IssueKeyInReason(reason)
	held, err := placeholdersOf(ctx, m.Store.Pool, conn.GrantID)
	if err != nil {
		return AirlockConnection{}, err
	}
	rows, err := m.Store.Pool.Query(ctx, `select name from withheld_secrets where enrollment_id=$1`, conn.EnrollmentID)
	if err != nil {
		return AirlockConnection{}, err
	}
	withheld, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return AirlockConnection{}, err
	}
	set := m.Policy.Get()
	conn.Names = map[string]AirlockGrantName{}
	for name, slots := range held {
		if slices.Contains(withheld, name) {
			continue
		}
		conn.Names[name] = AirlockGrantName{Slots: slots, Kind: set.Secrets[name].Kind()}
	}
	if len(conn.Names) == 0 {
		return AirlockConnection{}, ErrAirlockNoNames
	}
	return conn, nil
}

// issueKeyPattern is a Dispatch issue key: a project key — an uppercase letter and then uppercase
// letters or digits, as contracts.ProjectKeyPattern spells it — a hyphen, and the issue's number.
// The word boundaries keep a key out of the middle of a longer word, so a reason that names no
// key cannot be read as naming one.
var issueKeyPattern = regexp.MustCompile(`\b[A-Z][A-Z0-9]+-[0-9]+\b`)

// IssueKeyInReason is the first Dispatch issue key in a request's own signed reason, and "" when
// it names none. The proxy puts it in the line it logs for each request it carries a secret on,
// so an operator reading that line can open the issue the session said it was working on. It is
// exported for its own test; nothing outside this package calls it.
func IssueKeyInReason(reason string) string {
	return issueKeyPattern.FindString(reason)
}

// AirlockValues runs the full release checks of Values - the record chain, the policy
// re-evaluation, the max-grant, the airlock fingerprint - and answers each airlock name's real
// value. The proxy calls it on a grant's first connection on its own pod.
//
// It takes no enrollment: the connection's placeholder is what proves the grant, and
// AirlockConnection has already checked that the grant and its enrollment are live. A grant the
// checks refuse is ended exactly as Values ends it, since they are the same checks.
func (m *Machine) AirlockValues(ctx context.Context, grantID string) (map[string]string, error) {
	checked, err := m.checkRelease(ctx, grantID, "")
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	released := []string{}
	for _, g := range checked.granted {
		if m.Airlock.Lookup(g.name) == nil {
			continue
		}
		value, err := m.readValue(ctx, grantID, checked.enrollmentID, g, checked.set.Secrets[g.name], nil)
		if err != nil {
			return nil, err
		}
		values[g.name] = value
		released = append(released, g.name)
	}
	if err := m.recheckStoredSources(ctx, grantID, checked.granted); err != nil {
		return nil, err
	}
	// The same grant.used row a session's own release writes, marked as the proxy's: the values
	// did leave the broker, and an operator reading the trail must see that they did.
	if _, err := m.Store.Pool.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ('grant.used',$1,$2,$3,$4,$5)`,
		checked.enrollmentID, checked.requestID, grantID, "session:"+checked.enrollmentID,
		auditDetail{"names": released, "airlock": true}); err != nil {
		return nil, err
	}
	return values, nil
}
