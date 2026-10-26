// packages/envoy/cmd/agent-secrets/source.go
//
// Where an answer came from. A machine can hold more than one secrets CLI, each answering the
// same NAME from a store of its own, and a reader who takes one CLI's answer for another's acts
// on a value that was never there: on 2026-10-26 one name held one GitHub App's key in a local
// file and another App's key in this broker's store, and the agents that read the local one
// reported on the broker's. So every line this CLI prints about a secret names its own source,
// and so does every refusal, whether or not the command reached the broker at all.
//
// agent-secrets answers from the secrets broker AGENT_SECRETS_URL names and from nothing else:
// it reads no local secrets file. A source is therefore that broker, named by the variable that
// chose it, plus the kind of store the broker keeps the secrets in (GET /v1/settings: AWS Secrets
// Manager or GCP Secret Manager) wherever the CLI has read it. The settings read is best effort
// and bounded (describe): a broker that cannot be asked leaves a source naming the broker alone,
// which already tells one CLI's store from another's, rather than failing a command or guessing
// a store.
//
// A source as a session's forms print it (String) names no host, account, region or project:
// those lines land in an agent's command output and from there in whatever it quotes, a public
// pull request among them. Only the secret forms, which a person runs under their own cloud
// sign-in to write to one account or project, name it (storeName), and their --json carries it.
//
// No source names a secret's version: the broker releases a grant's values as names and values
// alone (POST /v1/grants/{id}/values), and a session holds no sign-in of its own to the store to
// read one with. `agent-secrets secret show` names each version, from the store itself under the
// person's own sign-in.
package main

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/redact"
)

// sourceSettingsTimeout bounds the settings read describe makes. It is short because naming the
// store is an addition to an answer the command already has: a slow broker must delay neither an
// injection nor a refusal.
const sourceSettingsTimeout = 5 * time.Second

// source is where this process's answers about a secret come from: the broker it asks, and that
// broker's own store where it is known.
type source struct {
	// host is the host (and port) of AGENT_SECRETS_URL, or that URL whole when it has none, for
	// the secret forms alone (brokerAt, storeName).
	host string
	// store is the broker's own store, as its settings describe it ("AWS Secrets Manager account
	// <id> in <region>", "GCP Secret Manager project <number>"), for the secret forms alone
	// (storeName); empty until describe has read them.
	store string
	// kind is the store's kind alone ("AWS Secrets Manager", "GCP Secret Manager"), which String
	// prints; empty until describe has read the settings.
	kind string
}

// brokerSource is the source naming the broker at base, with its store still unread.
func brokerSource(base string) source {
	return source{host: brokerHost(base)}
}

// brokerHost is base's host and port. A URL without a host (a bare host:port, say) is named
// whole, so the source always says which broker the command was pointed at, even one pointed at
// a value no URL parser reads; either way through redact.URL, so a credential in the variable
// never prints.
func brokerHost(base string) string {
	base = redact.URL(strings.TrimSuffix(base, "/"))
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return base
	}
	return parsed.Host
}

// describe is s with the broker's own store read onto it (GET /v1/settings, the public route
// every session may call), best effort: a broker that does not answer within
// sourceSettingsTimeout, or one too old to serve the route, leaves s as it was.
func (s source) describe(ctx context.Context, c *client) source {
	ctx, cancel := context.WithTimeout(ctx, sourceSettingsTimeout)
	defer cancel()
	settings, err := c.Settings(ctx)
	if err != nil {
		return s
	}
	return s.withSettings(settings)
}

// source is where c's own answers come from: the broker c calls, with that broker's store read
// onto it (describe). Every form that prints or refuses an answer about a secret names it, and
// calls this only on the path that needs it, so a form whose output is the broker's body
// verbatim (--json) makes no settings call at all.
func (c *client) source(ctx context.Context) source {
	return brokerSource(c.baseURL).describe(ctx, c)
}

// withSettings is s naming the store the broker's settings describe: its kind, and the account
// or project that holds the secrets, which is what tells two brokers' stores apart. A broker
// from before the `store` field names none and holds them in AWS Secrets Manager (Settings).
func (s source) withSettings(settings Settings) source {
	switch settings.Store {
	case "", secrets.StoreAWS:
		s.kind = "AWS Secrets Manager"
		s.store = s.kind
		if settings.AWSAccountID != "" {
			s.store += " account " + settings.AWSAccountID
		}
		if settings.AWSRegion != "" {
			s.store += " in " + settings.AWSRegion
		}
	case secrets.StoreGCP:
		s.kind = "GCP Secret Manager"
		s.store = s.kind
		if settings.GCPProject != "" {
			s.store += " project " + settings.GCPProject
		}
	default:
		// A store this release does not know: name it as the broker spells it rather than
		// leave the source silent about where the value came from.
		s.kind = settings.Store
		s.store = s.kind
	}
	return s
}

// String names the source in a sentence, as every form but the secret forms prints it: the
// broker AGENT_SECRETS_URL names, and its store's kind where it is known. It names no host,
// account, region or project (see the file comment), so two brokers of one kind read alike here;
// the line still says the answer came from this CLI's broker, not a local file.
func (s source) String() string {
	if s.kind == "" {
		return "the broker " + brokerURLVar + " names"
	}
	return "the broker " + brokerURLVar + " names (" + s.kind + ")"
}

// brokerAt names the broker by host, for the secret forms' lines, which name the store's account
// or project beside it (storeName).
func (s source) brokerAt() string {
	if s.host == "" {
		return "the broker"
	}
	return "the broker at " + s.host
}

// asked is what a refusal adds to say where this CLI looked, so a reader never takes a refusal
// of this CLI's for another CLI's answer about the same name.
func (s source) asked() string {
	return "; asked " + s.String()
}

// storeName is the store alone, for the secret forms: they read and write it directly under the
// person's own cloud sign-in rather than through the broker, so naming the broker as the source
// of those answers would be wrong. It falls back to naming the broker whose settings chose the
// store when those settings describe none this release knows how to name.
func (s source) storeName() string {
	if s.store == "" {
		return "the secret store " + s.brokerAt() + " names"
	}
	return s.store
}
