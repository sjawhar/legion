// Package rules parses agent-secret-rules.yaml (see the AGENTC-393 overview contract) and answers
// "what happens when this requester asks for this secret". Unknown keys, incomplete rules,
// entries no requester could ever satisfy, and two requester entries that match the same caller
// are refused at parse time, so a second entry can never silently remove an approval requirement
// and a typo can never leave a rule silently dead.
//
// The file also carries the approvers section: the WebAuthn origin, AAGUID allowlist, and the
// per-login attested key material an approver: login:<name> or approver: operator ultimately
// resolves to. Parse validates only that section's shape (required fields, formats, the
// exactly-one-of endorsement/seed constraint, and that every login a secret's rules name has a
// declared entry); it never verifies a registration's or endorsement's cryptographic attestation
// or checks whether a key is actually live in Postgres — the broker's approvers.Service.Reconcile
// (internal/broker/approvers) does that against the pinned trust roots and the persisted key set
// on every reload. The Python validator does both statically because it has no persisted set to
// check against; this package's Parse+Reconcile pair together cover the same checks Python does
// in one pass.
package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/record"
)

var ErrUnknownSecret = errors.New("no rule names this secret")

type file struct {
	Version   int                   `yaml:"version"`
	Secrets   map[string]secretRule `yaml:"secrets"`
	Approvers *approversSpec        `yaml:"approvers"`
}

type secretRule struct {
	Source             string       `yaml:"source"`
	Owner              string       `yaml:"owner"`
	Delivery           string       `yaml:"delivery"`
	MaxLifetimeSeconds int          `yaml:"max_lifetime_seconds"`
	Requesters         *[]requester `yaml:"requesters"` // pointer: an absent key is refused, an empty list denies everyone
	Proxy              *proxyRule   `yaml:"proxy"`
}

type requester struct {
	Kind     string `yaml:"kind"`
	Operator string `yaml:"operator"`
	// ServiceAccount scopes a pod entry to pods running as this Kubernetes service account
	// (system:serviceaccount:<namespace>:<name>); empty matches every pod.
	ServiceAccount string `yaml:"service_account"`
	Decision       string `yaml:"decision"`
	Approver       string `yaml:"approver"`
}

// serviceAccountSubject is the subject a projected service-account token carries.
var serviceAccountSubject = regexp.MustCompile(`^system:serviceaccount:[a-z0-9]([-a-z0-9]*[a-z0-9])?:[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`)

// challengeNonceHex is the required shape of an approver key entry's registration.challenge_nonce.
var challengeNonceHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type proxyRule struct {
	Scheme       string   `yaml:"scheme"`
	Host         string   `yaml:"host"`
	Port         int      `yaml:"port"`
	PathPrefix   string   `yaml:"path_prefix"`
	Methods      []string `yaml:"methods"`
	Header       string   `yaml:"header"`
	HeaderFormat string   `yaml:"header_format"`
}

// approversSpec is the wire shape of the file's top-level approvers: section.
type approversSpec struct {
	Origin  string               `yaml:"origin"`
	AAGUIDs []string             `yaml:"aaguids"`
	Logins  map[string]loginSpec `yaml:"logins"`
}

type loginSpec struct {
	Keys []keySpec `yaml:"keys"`
}

type keySpec struct {
	CredentialID string            `yaml:"credential_id"`
	Registration *registrationSpec `yaml:"registration"`
	Endorsement  *endorsementSpec  `yaml:"endorsement"`
	Seed         bool              `yaml:"seed"`
}

type registrationSpec struct {
	ChallengeNonce string `yaml:"challenge_nonce"`
	Response       any    `yaml:"response"`
}

type endorsementSpec struct {
	By        string `yaml:"by"`
	Assertion any    `yaml:"assertion"`
}

type Secret struct {
	Source      string
	Owner       string
	Delivery    string
	MaxLifetime time.Duration
	Requesters  []requester
	Proxy       *proxyRule
}

// Approvers is the parsed approvers section: the WebAuthn origin and AAGUID allowlist every
// registration is checked against, and the per-login attested key material an approver:
// login:<name> or approver: operator ultimately resolves to.
type Approvers struct {
	Origin  string
	AAGUIDs []uuid.UUID
	Logins  map[string][]approvers.KeyEntry
}

type Set struct {
	Version   string
	Secrets   map[string]Secret
	Approvers Approvers
}

type Requester struct {
	Kind     string
	Operator string
	// Subject is a pod's verified service-account subject, matched against an entry's
	// service_account.
	Subject string
}

type Decision struct {
	Outcome     string
	Approver    string
	Delivery    string
	Source      string
	MaxLifetime time.Duration
}

func Parse(data []byte) (*Set, error) {
	var f file
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	for {
		var extra yaml.Node
		err := dec.Decode(&extra)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("rules: %w", err)
		}
		if !isEmptyDocument(&extra) {
			return nil, fmt.Errorf("rules: file has more than one YAML document")
		}
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("rules: version must be 1, got %d", f.Version)
	}
	approversSet, err := parseApprovers(f.Approvers)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	set := &Set{Version: hex.EncodeToString(sum[:]), Secrets: map[string]Secret{}, Approvers: approversSet}
	for name, r := range f.Secrets {
		if r.Source == "" || r.Owner == "" {
			return nil, fmt.Errorf("rules: %s needs source and owner", name)
		}
		if r.Requesters == nil {
			return nil, fmt.Errorf("rules: %s needs a requesters list (empty means deny everyone)", name)
		}
		if r.Delivery != "inject" && r.Delivery != "proxy" {
			return nil, fmt.Errorf("rules: %s delivery must be inject or proxy", name)
		}
		if (r.Delivery == "proxy") != (r.Proxy != nil) {
			return nil, fmt.Errorf("rules: %s proxy block is required exactly when delivery is proxy", name)
		}
		if r.MaxLifetimeSeconds < 1 || r.MaxLifetimeSeconds > 43200 {
			return nil, fmt.Errorf("rules: %s max_lifetime_seconds must be 1..43200", name)
		}
		seen := map[string]bool{}
		requesters := make([]requester, 0, len(*r.Requesters))
		pods, podsForAnyAccount := 0, false
		for i, q := range *r.Requesters {
			q.Operator = record.CanonicalLogin(q.Operator)
			q.ServiceAccount = strings.TrimSpace(q.ServiceAccount)
			switch q.Kind {
			case "box", "host":
				if q.Operator == "" {
					return nil, fmt.Errorf("rules: %s requesters[%d]: %s needs operator", name, i, q.Kind)
				}
				if q.ServiceAccount != "" {
					return nil, fmt.Errorf("rules: %s requesters[%d]: service_account applies only to pod", name, i)
				}
			case "pod":
				if q.Operator != "" {
					return nil, fmt.Errorf("rules: %s requesters[%d]: pod takes no operator", name, i)
				}
				if q.ServiceAccount != "" && !serviceAccountSubject.MatchString(q.ServiceAccount) {
					return nil, fmt.Errorf("rules: %s requesters[%d]: service_account must be system:serviceaccount:<namespace>:<name>, got %q", name, i, q.ServiceAccount)
				}
				pods++
				podsForAnyAccount = podsForAnyAccount || q.ServiceAccount == ""
			default:
				return nil, fmt.Errorf("rules: %s requesters[%d]: kind must be box, host or pod", name, i)
			}
			switch q.Decision {
			case "automatic", "deny":
				if q.Approver != "" {
					return nil, fmt.Errorf("rules: %s requesters[%d]: approver only with decision approval", name, i)
				}
			case "approval":
				switch {
				case q.Approver == "operator" && q.Kind == "pod":
					return nil, fmt.Errorf("rules: %s requesters[%d]: a pod has no operator to approve; use login:<name>", name, i)
				case q.Approver == "operator":
					if _, ok := set.Approvers.Logins[q.Operator]; !ok {
						return nil, fmt.Errorf("rules: %s: approver operator %s has no approvers entry", name, q.Operator)
					}
				case strings.HasPrefix(q.Approver, "login:"):
					login := record.CanonicalLogin(strings.TrimPrefix(q.Approver, "login:"))
					if login == "" {
						return nil, fmt.Errorf("rules: %s requesters[%d]: approver login: names nobody", name, i)
					}
					if _, ok := set.Approvers.Logins[login]; !ok {
						return nil, fmt.Errorf("rules: %s: approver login:%s has no approvers entry", name, login)
					}
					q.Approver = "login:" + login
				default:
					return nil, fmt.Errorf("rules: %s requesters[%d]: approver must be operator or login:<name>", name, i)
				}
			default:
				return nil, fmt.Errorf("rules: %s requesters[%d]: decision must be automatic, approval or deny", name, i)
			}
			key := q.Kind + "/" + q.Operator + "/" + q.ServiceAccount
			if seen[key] {
				return nil, fmt.Errorf("rules: %s: ambiguous requester %s matches two entries", name, key)
			}
			seen[key] = true
			requesters = append(requesters, q)
		}
		if podsForAnyAccount && pods > 1 {
			return nil, fmt.Errorf("rules: %s: ambiguous requester: a pod entry without service_account matches every pod another pod entry names", name)
		}
		set.Secrets[name] = Secret{Source: r.Source, Owner: r.Owner, Delivery: r.Delivery,
			MaxLifetime: time.Duration(r.MaxLifetimeSeconds) * time.Second, Requesters: requesters, Proxy: r.Proxy}
	}
	return set, nil
}

// parseApprovers validates and converts the file's approvers: section. An absent section is
// valid and produces a zero-value Approvers (Logins nil): a rules file with no approval-decision
// requesters needs no approvers section at all, and any login:/operator: approver naming a login
// absent from a nil Logins map is refused the same way as one absent from an empty map. Every
// check here is structural; see the package doc comment for the shape/crypto split.
func parseApprovers(spec *approversSpec) (Approvers, error) {
	if spec == nil {
		return Approvers{}, nil
	}
	parsedOrigin, err := url.Parse(spec.Origin)
	if err != nil || parsedOrigin.Scheme != "https" || parsedOrigin.Host == "" || parsedOrigin.Path != "" {
		return Approvers{}, fmt.Errorf("rules: approvers.origin must be an absolute https URL with no path, got %q", spec.Origin)
	}
	if len(spec.AAGUIDs) == 0 {
		return Approvers{}, fmt.Errorf("rules: approvers.aaguids must be non-empty")
	}
	aaguids := make([]uuid.UUID, 0, len(spec.AAGUIDs))
	for _, s := range spec.AAGUIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			return Approvers{}, fmt.Errorf("rules: approvers.aaguids: %q is not a valid UUID", s)
		}
		aaguids = append(aaguids, id)
	}
	logins := make(map[string][]approvers.KeyEntry, len(spec.Logins))
	for login, l := range spec.Logins {
		canon := record.CanonicalLogin(login)
		if canon == "" {
			return Approvers{}, fmt.Errorf("rules: approvers.logins: a login key must not be blank")
		}
		if _, dup := logins[canon]; dup {
			return Approvers{}, fmt.Errorf("rules: approvers.logins: %s and another entry both name login %s", login, canon)
		}
		entries := make([]approvers.KeyEntry, 0, len(l.Keys))
		for i, k := range l.Keys {
			if k.CredentialID == "" {
				return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: credential_id is required", login, i)
			}
			if k.Registration == nil {
				return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: registration is required", login, i)
			}
			if !challengeNonceHex.MatchString(k.Registration.ChallengeNonce) {
				return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: registration.challenge_nonce must be 64 hex characters", login, i)
			}
			if k.Registration.Response == nil {
				return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: registration.response is required", login, i)
			}
			response, err := json.Marshal(k.Registration.Response)
			if err != nil {
				return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: registration.response: %w", login, i, err)
			}
			if (k.Endorsement != nil) == k.Seed {
				return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: exactly one of endorsement or seed is required", login, i)
			}
			entry := approvers.KeyEntry{
				CredentialID:   k.CredentialID,
				ChallengeNonce: k.Registration.ChallengeNonce,
				Registration:   response,
				Seed:           k.Seed,
			}
			if k.Endorsement != nil {
				if k.Endorsement.By == "" {
					return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: endorsement.by is required", login, i)
				}
				if k.Endorsement.By == k.CredentialID {
					return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: endorsement.by must not name the key's own credential_id", login, i)
				}
				if k.Endorsement.Assertion == nil {
					return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: endorsement.assertion is required", login, i)
				}
				assertion, err := json.Marshal(k.Endorsement.Assertion)
				if err != nil {
					return Approvers{}, fmt.Errorf("rules: approvers.logins.%s.keys[%d]: endorsement.assertion: %w", login, i, err)
				}
				entry.Endorsement = &approvers.Endorsement{By: k.Endorsement.By, Assertion: assertion}
			}
			entries = append(entries, entry)
		}
		logins[canon] = entries
	}
	return Approvers{Origin: spec.Origin, AAGUIDs: aaguids, Logins: logins}, nil
}

// isEmptyDocument reports whether a decoded yaml.Node is the trailing "---" YAML.v3 emits for a
// document separator with nothing meaningful after it (no content, or a lone null scalar) rather
// than a genuine second document, which Parse must refuse.
func isEmptyDocument(n *yaml.Node) bool {
	if len(n.Content) == 0 {
		return true
	}
	return len(n.Content) == 1 && n.Content[0].Kind == yaml.ScalarNode && n.Content[0].Tag == "!!null"
}

func (s *Set) Evaluate(name string, r Requester) (Decision, error) {
	secret, ok := s.Secrets[name]
	if !ok {
		return Decision{}, ErrUnknownSecret
	}
	d := Decision{Outcome: "deny", Delivery: secret.Delivery, Source: secret.Source, MaxLifetime: secret.MaxLifetime}
	operator := record.CanonicalLogin(r.Operator)
	for _, q := range secret.Requesters {
		switch {
		case q.Kind != r.Kind:
			continue
		case q.Kind != "pod" && q.Operator != operator:
			continue
		case q.Kind == "pod" && q.ServiceAccount != "" && q.ServiceAccount != r.Subject:
			continue
		}
		d.Outcome = q.Decision
		switch {
		case q.Decision != "approval":
		case q.Approver == "operator":
			d.Approver = operator
		default:
			d.Approver = strings.TrimPrefix(q.Approver, "login:")
		}
		if d.Outcome == "approval" && len(s.Approvers.Logins[d.Approver]) == 0 {
			d.Approver = "" // no declared (hence no live) keys for this login: nobody to approve
		}
		if d.Outcome == "approval" && d.Approver == "" {
			d.Outcome = "deny" // an approval with nobody to approve it is a refusal, never a silent grant
		}
		return d, nil
	}
	return d, nil
}
