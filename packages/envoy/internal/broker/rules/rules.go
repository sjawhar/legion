// Package rules parses agent-secret-rules.yaml (packages/envoy/AGENTS.md describes it) and answers
// "what happens when this requester asks for this secret". Unknown keys, incomplete rules,
// entries no requester could ever satisfy, and two requester entries that match the same caller
// are refused at parse time, so a second entry can never silently remove an approval requirement
// and a typo can never leave a rule silently dead.
//
// An approval rule names who decides: approver: login:<name>, or approver: operator for the
// requesting enrollment's own operator. That login decides through Dispatch; the file carries no
// key material for it, and a file that still has the removed approvers: section is refused
// naming the removal.
package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/broker/record"
)

var ErrUnknownSecret = errors.New("no rule names this secret")

type file struct {
	Version int                   `yaml:"version"`
	Secrets map[string]secretRule `yaml:"secrets"`
	// Approvers is decoded only to refuse it: the section held WebAuthn approver keys, which
	// approval by Dispatch login removed. A yaml.Node takes any content, so the
	// refusal below names the removal whatever the section holds; its Kind is zero only when the
	// file has no approvers key at all.
	Approvers yaml.Node `yaml:"approvers"`
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

type proxyRule struct {
	Scheme       string   `yaml:"scheme"`
	Host         string   `yaml:"host"`
	Port         int      `yaml:"port"`
	PathPrefix   string   `yaml:"path_prefix"`
	Methods      []string `yaml:"methods"`
	Header       string   `yaml:"header"`
	HeaderFormat string   `yaml:"header_format"`
}

type Secret struct {
	Source      string
	Owner       string
	Delivery    string
	MaxLifetime time.Duration
	Requesters  []requester
	Proxy       *proxyRule
}

type Set struct {
	Version string
	Secrets map[string]Secret
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
	if f.Approvers.Kind != 0 {
		return nil, errors.New("rules: the approvers: section is removed; approval is by Dispatch login and the broker keeps no approver keys, so delete the section")
	}
	sum := sha256.Sum256(data)
	set := &Set{Version: hex.EncodeToString(sum[:]), Secrets: map[string]Secret{}}
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
				case strings.HasPrefix(q.Approver, "login:"):
					login := record.CanonicalLogin(strings.TrimPrefix(q.Approver, "login:"))
					if login == "" {
						return nil, fmt.Errorf("rules: %s requesters[%d]: approver login: names nobody", name, i)
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
		if d.Outcome == "approval" && d.Approver == "" {
			d.Outcome = "deny" // an approval with nobody to approve it is a refusal, never a silent grant
		}
		return d, nil
	}
	return d, nil
}
