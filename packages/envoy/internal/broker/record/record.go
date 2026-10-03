// Package record implements the credential-request record: its canonical body and content-addressed
// id, who may decide it, and verification of the requester's signed request object (AGENTC-393
// design v4, and the shared broker contract at dispatch://AGENTC-393/artifact/plan-overview-md).
package record

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-jose/go-jose/v4"
	"github.com/sjawhar/envoy/internal/broker/proof"
)

// RequestTyp is the JWS "typ" header for a credential-request object. It is disjoint from
// proof's "agent-secrets-proof+jwt": each verifier refuses the other's typ.
const RequestTyp = "agent-secrets-request+jwt"

// canonicalHeader is the first line of every canonical body.
const canonicalHeader = "agent-secrets-record/v1"

// maxRequestLifetime bounds how far past iat a request object's exp may sit.
const maxRequestLifetime = 600 * time.Second

// ErrRequestInvalid is wrapped around every request-object verification failure. The wrapped
// reason never quotes the JWS.
var ErrRequestInvalid = errors.New("request object invalid")

var (
	hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*$`)
	servicePattern  = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	slotPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// ValidSlot reports whether slot may name a pod enrollment's slot: one of several independent
// identities in one pod, such as a Legion role and its generation. The empty slot is the
// enrollment's one identity and is not a slot value.
func ValidSlot(slot string) bool {
	return slotPattern.MatchString(slot)
}

// CanonicalLogin lowercases and trims the name Dispatch signs a person in with, their email (a
// record created before people were named by email keeps the GitHub login it was decided under).
// Every login comparison in the module goes through this form on both sides.
func CanonicalLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// AnyoneApprover is the approver of a record anyone signed in to Dispatch may decide: a request
// for a shared human-tier secret. It is never a person's login, and no login is it.
const AnyoneApprover = "anyone"

// AuthorizationDetail is one entry of a request object's RFC 9396 authorization_details.
type AuthorizationDetail struct {
	Type       string   `json:"type"` // "agent_secret" | "launcher_credential"
	Identifier string   `json:"identifier"`
	Actions    []string `json:"actions,omitempty"` // ["inject"] for agent_secret
	Service    string   `json:"service,omitempty"`
}

// RequestObject is the verified claims of a credential-request object plus the verbatim compact
// JWS it came from.
type RequestObject struct {
	Compact    string
	Thumbprint string // iss, equal to the embedded JWK's RFC 7638 thumbprint
	Audience   string
	JTI        string
	IssuedAt   time.Time
	Expires    time.Time
	Details    []AuthorizationDetail
	Reason     string
	LoginHint  string // machine logins only
}

// requestClaims is the JSON payload of a credential-request object (RFC 9101 §4 shape, RAR
// authorization_details per RFC 9396 §2).
type requestClaims struct {
	Issuer               string                `json:"iss"`
	Audience             string                `json:"aud"`
	JTI                  string                `json:"jti"`
	IssuedAt             int64                 `json:"iat"`
	Expires              int64                 `json:"exp"`
	AuthorizationDetails []AuthorizationDetail `json:"authorization_details"`
	Reason               string                `json:"reason,omitempty"`
	LoginHint            string                `json:"login_hint,omitempty"`
}

// VerifyRequestObject enforces the contract: single ES256 JWS, typ RequestTyp, embedded P-256 JWK,
// iss == thumbprint(jwk), aud == audience, iat within skew, exp in (now, iat+10m], reason ≤400 runes
// with categories Cc/Cf/Cs/Co/Zl/Zp refused, details non-empty and single-typed, launcher identifiers
// matching the contract hostname pattern, service matching [a-z0-9-]{1,64}. Replay (jti) is the
// caller's (it needs the store). Every failure wraps ErrRequestInvalid with a reason that never
// quotes the JWS.
func VerifyRequestObject(compact, audience string, skew time.Duration, now time.Time) (RequestObject, error) {
	sig, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(sig.Signatures) != 1 {
		return RequestObject{}, fmt.Errorf("%w: not a single ES256 JWS", ErrRequestInvalid)
	}
	header := sig.Signatures[0].Protected
	if typ, _ := header.ExtraHeaders[jose.HeaderType].(string); typ != RequestTyp {
		return RequestObject{}, fmt.Errorf("%w: typ is not %s", ErrRequestInvalid, RequestTyp)
	}
	if header.JSONWebKey == nil {
		return RequestObject{}, fmt.Errorf("%w: no embedded key", ErrRequestInvalid)
	}
	pub, ok := header.JSONWebKey.Key.(*ecdsa.PublicKey)
	if !ok || pub.Curve.Params().Name != "P-256" {
		return RequestObject{}, fmt.Errorf("%w: key is not EC P-256", ErrRequestInvalid)
	}
	payload, err := sig.Verify(pub)
	if err != nil {
		return RequestObject{}, fmt.Errorf("%w: signature", ErrRequestInvalid)
	}
	var c requestClaims
	if err := json.Unmarshal(payload, &c); err != nil || c.JTI == "" {
		return RequestObject{}, fmt.Errorf("%w: claims", ErrRequestInvalid)
	}
	thumbprint, err := proof.Thumbprint(pub)
	if err != nil || c.Issuer == "" || c.Issuer != thumbprint {
		return RequestObject{}, fmt.Errorf("%w: iss is not the embedded key's thumbprint", ErrRequestInvalid)
	}
	if c.Audience == "" || c.Audience != audience {
		return RequestObject{}, fmt.Errorf("%w: aud does not match", ErrRequestInvalid)
	}
	issued := time.Unix(c.IssuedAt, 0)
	if issued.After(now.Add(skew)) || issued.Before(now.Add(-skew)) {
		return RequestObject{}, fmt.Errorf("%w: iat outside %s", ErrRequestInvalid, skew)
	}
	expires := time.Unix(c.Expires, 0)
	if !expires.After(now) || expires.After(issued.Add(maxRequestLifetime)) {
		return RequestObject{}, fmt.Errorf("%w: exp outside (now, iat+%s]", ErrRequestInvalid, maxRequestLifetime)
	}
	if err := validateDetails(c.AuthorizationDetails); err != nil {
		return RequestObject{}, fmt.Errorf("%w: %s", ErrRequestInvalid, err)
	}
	if err := validateReason(c.Reason); err != nil {
		return RequestObject{}, fmt.Errorf("%w: %s", ErrRequestInvalid, err)
	}
	return RequestObject{
		Compact:    compact,
		Thumbprint: c.Issuer,
		Audience:   c.Audience,
		JTI:        c.JTI,
		IssuedAt:   issued,
		Expires:    expires,
		Details:    c.AuthorizationDetails,
		Reason:     c.Reason,
		LoginHint:  c.LoginHint,
	}, nil
}

// validateDetails enforces: non-empty, and single-typed — every entry "agent_secret", or exactly
// one "launcher_credential" entry whose identifier is a valid hostname and whose optional service
// matches [a-z0-9-]{1,64}.
func validateDetails(details []AuthorizationDetail) error {
	if len(details) == 0 {
		return errors.New("authorization_details is empty")
	}
	allSecret := true
	for _, d := range details {
		if d.Type != "agent_secret" {
			allSecret = false
			break
		}
	}
	if allSecret {
		for _, d := range details {
			if d.Identifier == "" {
				return errors.New("agent_secret detail has no identifier")
			}
		}
		return nil
	}
	if len(details) != 1 || details[0].Type != "launcher_credential" {
		return errors.New("authorization_details mixes types")
	}
	d := details[0]
	if len(d.Identifier) > 253 || !hostnamePattern.MatchString(d.Identifier) {
		return errors.New("launcher_credential identifier is not a valid hostname")
	}
	if d.Service != "" && !servicePattern.MatchString(d.Service) {
		return errors.New("launcher_credential service does not match [a-z0-9-]{1,64}")
	}
	return nil
}

// validateReason enforces: at most 400 runes, none in Unicode categories Cc, Cf, Cs, Co, Zl, or Zp
// (bidi overrides and zero-width characters are Cf).
func validateReason(reason string) error {
	if utf8.RuneCountInString(reason) > 400 {
		return errors.New("reason is over 400 runes")
	}
	for _, r := range reason {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Co, unicode.Zl, unicode.Zp) {
			return errors.New("reason contains a refused rune category")
		}
	}
	return nil
}

// Enrollment identifies the enrollment a credential request came from. A machine login (no
// enrollment) carries "-", "-", "-". Slot is a pod enrollment's slot, "" for every enrollment
// without one.
type Enrollment struct{ Kind, RuntimeID, Operator, Slot string }

// Body is the credential-request record's decision fields, fixed at creation, plus the verbatim
// signed request object. It is never updated after creation.
type Body struct {
	Request         string // compact JWS
	Approver        string // canonical login, or AnyoneApprover
	Enrollment      Enrollment
	LifetimeSeconds int
	RulesVersion    string
	ExpiresAt       time.Time // UTC, seconds precision
	Code            string    // "" for agent_secret records
}

// Canonical renders the body in the contract's exact line format. Every line is "\n"-terminated;
// an empty operator or code renders as "-". The enrollment line carries a pod's slot as a fourth
// tab-separated field only when there is one: a body with no slot keeps the three-field line, the
// bytes and record id every slotless record is stored under.
func (b Body) Canonical() string {
	operator := b.Enrollment.Operator
	if operator == "" {
		operator = "-"
	}
	code := b.Code
	if code == "" {
		code = "-"
	}
	var sb strings.Builder
	sb.WriteString(canonicalHeader)
	sb.WriteByte('\n')
	sb.WriteString("request: ")
	sb.WriteString(b.Request)
	sb.WriteByte('\n')
	sb.WriteString("approver: ")
	sb.WriteString(b.Approver)
	sb.WriteByte('\n')
	sb.WriteString("enrollment: ")
	sb.WriteString(b.Enrollment.Kind)
	sb.WriteByte('\t')
	sb.WriteString(b.Enrollment.RuntimeID)
	sb.WriteByte('\t')
	sb.WriteString(operator)
	if b.Enrollment.Slot != "" {
		sb.WriteByte('\t')
		sb.WriteString(b.Enrollment.Slot)
	}
	sb.WriteByte('\n')
	sb.WriteString("lifetime_seconds: ")
	sb.WriteString(strconv.Itoa(b.LifetimeSeconds))
	sb.WriteByte('\n')
	sb.WriteString("rules_version: ")
	sb.WriteString(b.RulesVersion)
	sb.WriteByte('\n')
	sb.WriteString("expires_at: ")
	sb.WriteString(b.ExpiresAt.UTC().Format(time.RFC3339))
	sb.WriteByte('\n')
	sb.WriteString("code: ")
	sb.WriteString(code)
	sb.WriteByte('\n')
	return sb.String()
}

// ID is the lowercase-hex SHA-256 of the body's canonical form.
func (b Body) ID() string {
	sum := sha256.Sum256([]byte(b.Canonical()))
	return hex.EncodeToString(sum[:])
}

// canonicalLines is the fixed number of "\n"-terminated lines Canonical produces (the trailing
// split element after the last "\n" is the ninth, empty, element).
const canonicalLines = 9

// ParseBody is the exact inverse of Canonical: it refuses any deviation from the fixed line
// format, including a value that would still parse but not reproduce the input byte-for-byte.
func ParseBody(canonical string) (Body, error) {
	lines := strings.Split(canonical, "\n")
	if len(lines) != canonicalLines || lines[canonicalLines-1] != "" {
		return Body{}, fmt.Errorf("canonical body: wrong line count")
	}
	if lines[0] != canonicalHeader {
		return Body{}, fmt.Errorf("canonical body: bad header line")
	}
	request, ok := strings.CutPrefix(lines[1], "request: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad request line")
	}
	approver, ok := strings.CutPrefix(lines[2], "approver: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad approver line")
	}
	enrollmentLine, ok := strings.CutPrefix(lines[3], "enrollment: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad enrollment line")
	}
	fields := strings.Split(enrollmentLine, "\t")
	if len(fields) != 3 && len(fields) != 4 {
		return Body{}, fmt.Errorf("canonical body: enrollment is not three or four tab-separated fields")
	}
	slot := ""
	if len(fields) == 4 {
		slot = fields[3]
		if fields[0] != "pod" || !ValidSlot(slot) {
			return Body{}, fmt.Errorf("canonical body: an enrollment's fourth field must be a pod's slot")
		}
	}
	operator := fields[2]
	if operator == "-" {
		operator = ""
	}
	lifetimeStr, ok := strings.CutPrefix(lines[4], "lifetime_seconds: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad lifetime_seconds line")
	}
	lifetime, err := strconv.Atoi(lifetimeStr)
	if err != nil {
		return Body{}, fmt.Errorf("canonical body: lifetime_seconds is not an integer")
	}
	rulesVersion, ok := strings.CutPrefix(lines[5], "rules_version: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad rules_version line")
	}
	expiresStr, ok := strings.CutPrefix(lines[6], "expires_at: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad expires_at line")
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return Body{}, fmt.Errorf("canonical body: expires_at is not RFC3339")
	}
	codeStr, ok := strings.CutPrefix(lines[7], "code: ")
	if !ok {
		return Body{}, fmt.Errorf("canonical body: bad code line")
	}
	code := codeStr
	if code == "-" {
		code = ""
	}
	b := Body{
		Request:         request,
		Approver:        approver,
		Enrollment:      Enrollment{Kind: fields[0], RuntimeID: fields[1], Operator: operator, Slot: slot},
		LifetimeSeconds: lifetime,
		RulesVersion:    rulesVersion,
		ExpiresAt:       expiresAt.UTC(),
		Code:            code,
	}
	if b.Canonical() != canonical {
		return Body{}, fmt.Errorf("canonical body: does not round-trip")
	}
	return b, nil
}

// ApproverLogin canonicalizes login and returns it when it may decide this record, and
// ErrNotApprover otherwise: login must be the approver the record names, or, when that is
// AnyoneApprover, any login at all. A record's approver is resolved when it is created — a
// secret's owner, AnyoneApprover for a shared human-tier secret, or a machine login's login_hint —
// so this one comparison is every decision's and every chain re-check's approver rule, and the
// login it returns is the one a decision records.
func (b Body) ApproverLogin(login string) (string, error) {
	login = CanonicalLogin(login)
	approver := CanonicalLogin(b.Approver)
	if login == "" || login == AnyoneApprover || login != approver && approver != AnyoneApprover {
		return "", ErrNotApprover
	}
	return login, nil
}

// isApprover reports whether login is the approver this record names, by ApproverLogin's rule.
func (b Body) isApprover(login string) bool {
	_, err := b.ApproverLogin(login)
	return err == nil
}
