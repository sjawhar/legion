// Package policy decides who may have an agent secret from two tags on the secret itself in the
// secret store (AWS Secrets Manager's tags, or Google Secret Manager's annotations): owner
// (shared, a person's email, or a registered service) and tier (agent or human). Loader reads every secret under the namespace prefix into a Set, refusing by name each one
// whose tags or key the policy cannot serve, or reads one name alone (LoadOne); Current keeps the
// latest Set, rereads it on a ticker, and merges a single name's reread into it (RefreshOne);
// Set.Evaluate answers one requester's ask for one name.
//
// Who may ask follows from owner and tier, except where a secret's requesters tag names the only
// people and services that may ask for it at all (TagRequesters, checked before everything below,
// and a session or pod it does not name is denied whatever the owner and tier would have given
// it):
//
//   - a person's agent-tier secret goes to that person's sessions without asking, and any other
//     session's or pod's request is an approval request to the person;
//   - a shared agent-tier secret goes to any session or pod;
//   - a human-tier secret needs its owner's approval, or anyone's if it is shared;
//   - a service's secret goes only to that service's own sessions, and anyone else's request is
//     refused;
//   - a secret a person withheld from a session, by revoking a grant of it the session got
//     without asking, is human tier to that session: it asks its owner, or anyone for a shared
//     secret, and a service's own secret is refused.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// The tags a namespace secret carries, and their fixed values.
const (
	// TagOwner names who owns the secret: OwnerShared, a person's lowercase email, or a registered
	// service's name.
	TagOwner = "owner"
	// TagTier is TierAgent or TierHuman.
	TagTier = "tier"
	// TagKind selects the token type a secret mints on release. An absent or empty tag leaves the
	// secret's stored value unchanged.
	TagKind = "kind"
	// KindGitHubApp mints one installation token for every installation of the stored GitHub App
	// on an account its TagGitHubOwners lists.
	KindGitHubApp = "github-app"
	// KindGoogleLogin exchanges the stored authorized-user refresh token for an access token.
	KindGoogleLogin = "google-login"
	// KindAzureLogin releases the broker's projected token with the stored Entra identity ids.
	KindAzureLogin = "azure-login"
	// KindGoogleServiceAccount mints a Google access token for the one service account its stored
	// value names, {"service_account": "<id>@<project>.iam.gserviceaccount.com"}. The token acts
	// as that account, which no person's sign-in bounds, so the kind is served only to the one
	// person the secret's requesters tag names, beside the registered services that tag lists at
	// human tier, and only for grants of an hour at most (TagRules). The stored value does not
	// choose the account alone: the deployment's BROKER_SERVICE_ACCOUNT_SECRETS binds each such
	// secret to one account email, and a mint for any other account is refused, so a value
	// rewritten to name another account releases no token.
	KindGoogleServiceAccount = "google-service-account"
	// TagMaxGrant bounds a request's signed grant lifetime for this secret (ParseMaxGrant).
	TagMaxGrant = "max-grant"
	// TagDefaultGrant, set to DefaultGrantSession, keeps a request that names no lifetime for this
	// secret lasting until its session ends, rather than the broker's default grant lifetime.
	TagDefaultGrant = "default-grant"
	// DefaultGrantSession is the one value TagDefaultGrant takes.
	DefaultGrantSession = "session"
	// TagGitHubOwners lists the GitHub accounts a github-app secret mints installation tokens for
	// (ParseGitHubOwners). A github-app secret without it is refused, since a public App can be
	// installed by any account, and an installation on an account it does not list gets no token.
	TagGitHubOwners = "github-owners"
	// TagRequesters lists who may ask for the secret at all: people by lowercase email and
	// registered services by name, separated by white space (ParseRequesters). A session or pod
	// the list does not name is denied before owner and tier are read (ReasonRequesterNotListed),
	// so the tag narrows who may ask and never widens it. It bounds no grant by itself: the
	// hour-long cap is the business of TagAdmin and of the google-service-account kind, each of
	// which demands one (TagRules).
	TagRequesters = "requesters"
	// TagScopes lists the OAuth scope names of the token a google-service-account secret mints,
	// separated by white space, each the part of a Google scope that follows
	// https://www.googleapis.com/auth/ (ParseScopes, Secret.ScopeURLs puts the prefix back). A
	// google-service-account secret without it mints the cloud-platform scope alone, which is
	// what every such secret minted before the tag existed. The names are short because the
	// broker's AWS store keeps its annotations as AWS tags, an AWS tag value holds at most 256
	// characters, and five full scope URLs are longer than that. The tag on a secret of any
	// other kind is refused (ReasonScopesKind), since no other minter reads it.
	TagScopes = "scopes"
	// TagAdmin marks a secret whose grant is an administrator's: "true" is the only value the
	// loader accepts, and anything else is refused (ReasonAdminMalformed). The broker cannot tell
	// such a secret from its kind — an admin console password and a personal API key are both
	// stored strings — so the program that declares the secret says so. An admin secret must be
	// human tier, carry a requesters list and carry a max-grant of an hour or less, so every
	// release of it is approved by a person, asked for by someone it names, and short enough to
	// be reviewed while it is live (TagRules).
	TagAdmin = "admin"
	// AdminTrue is the one value TagAdmin takes, as DefaultGrantSession is TagDefaultGrant's: the
	// loader refuses every other value (ReasonAdminMalformed), and `agent-secrets` holds --admin
	// to it before it writes, so the two cannot drift on what the tag says.
	AdminTrue = "true"
	// kindSourcePrefix opens the Source of a kind secret (KindSource). The kind and store source
	// are frozen into a request, so changing or clearing the kind ends every live grant.
	kindSourcePrefix = "mint:"
	// TagRemoved is the value of `agent-secrets secret retag --kind`, `--github-owners`,
	// `--requesters` and `--max-grant` that removes the tag. It is never a login a github-owners
	// tag names: an entry that reads `none` in any case was written by hand, and is refused
	// rather than read as an account (ParseGitHubOwners).
	TagRemoved = "none"
	// TagRollbackGuard and RollbackGuardValue are the tag `agent-secrets secret` writes beside every
	// kind, and the value it writes. This broker never reads the tag. Brokers from
	// legion-envoy-v13.19.0 to the release before kinds read it as the service account a secret
	// mints for, and refuse a value that is not a service account's email (mint-target-malformed),
	// so a rollback to one of them refuses a kind secret rather than serving its stored App key or
	// refresh token. Brokers older than legion-envoy-v13.19.0 read no such tag and serve it; never
	// run one against a store that holds kind secrets.
	TagRollbackGuard   = "mint-service-account"
	RollbackGuardValue = "kind-secret"
	// OwnerShared is the owner value of a secret nobody owns alone.
	OwnerShared = "shared"
	// TierAgent is a secret its owner's agents use without asking.
	TierAgent = "agent"
	// TierHuman is a secret every request for which a person approves.
	TierHuman = "human"
)

// The outcomes Evaluate answers, stored as each requested name's decision.
const (
	Automatic = "automatic"
	Approval  = "approval"
	Deny      = "deny"
)

// Kinds is every value a secret's kind tag may name. The broker holds a minter for each.
var Kinds = []string{KindGitHubApp, KindGoogleLogin, KindAzureLogin, KindGoogleServiceAccount}

// ValidKind reports whether kind is one of Kinds.
func ValidKind(kind string) bool {
	return slices.Contains(Kinds, kind)
}

// ErrMaxGrantMalformed is a max-grant tag that is not a Go duration of at least one second.
var ErrMaxGrantMalformed = errors.New("max-grant is a duration of at least 1s")

// ParseMaxGrant reads a max-grant tag: a Go duration (time.ParseDuration) of at least one second,
// so 0 is free to mean that a secret sets none.
func ParseMaxGrant(raw string) (time.Duration, error) {
	maxGrant, err := time.ParseDuration(raw)
	if err != nil || maxGrant < time.Second {
		return 0, ErrMaxGrantMalformed
	}
	return maxGrant, nil
}

// ErrGitHubOwnersMissing is a github-owners tag that names no account.
var ErrGitHubOwnersMissing = errors.New("github-owners names at least one GitHub account")

// ErrGitHubOwnersComma is a github-owners tag that holds a comma: its logins are separated by
// spaces, as BROKER_SERVICES separates its entries, since an AWS tag value takes a space and no
// comma.
var ErrGitHubOwnersComma = errors.New(`github-owners separates GitHub logins with spaces, not commas ("<login> <login>")`)

// ErrGitHubOwnersLogin is a github-owners entry that cannot be a GitHub login: one that does not
// start with an ASCII letter or digit, holds anything but ASCII letters, digits and hyphens, is
// longer than 39 characters, or is the word none.
var ErrGitHubOwnersLogin = errors.New("github-owners names GitHub logins: up to 39 ASCII letters, digits and hyphens, starting with a letter or digit")

// gitHubLogin is what a GitHub login can be: an ASCII letter or digit, then ASCII letters, digits
// and hyphens. It is matched against the entry as written, before it is lowercased, since Unicode
// lowercasing maps some non-ASCII letters onto ASCII ones (U+212A KELVIN SIGN to `k`, U+0130 to
// `i` with a combining dot). GitHub's sign-up rule today also refuses a trailing or doubled hyphen,
// but accounts made under its older rules keep them (`x-` is an organization from 2011), so this
// refuses only what no login can hold. A login is at most 39 characters (maxGitHubLogin), which
// the pattern does not check.
var gitHubLogin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

const maxGitHubLogin = 39

// ParseGitHubLogin reads one GitHub account login, as written: up to 39 ASCII letters, digits and
// hyphens, starting with a letter or digit, and not the word none (ErrGitHubOwnersLogin). It
// answers it lowercased, since GitHub compares logins without regard to case. White space is no
// part of a login, so "ada ada" is refused rather than read as one.
func ParseGitHubLogin(raw string) (string, error) {
	if len(raw) > maxGitHubLogin || !gitHubLogin.MatchString(raw) {
		return "", fmt.Errorf("%w, not %q", ErrGitHubOwnersLogin, raw)
	}
	login := strings.ToLower(raw)
	if login == TagRemoved {
		return "", fmt.Errorf("%w, not %q: none removes the tag through agent-secrets and names no account", ErrGitHubOwnersLogin, raw)
	}
	return login, nil
}

// ParseGitHubOwners reads a github-owners tag: GitHub account logins separated by white space,
// compared without regard to case. It refuses a comma anywhere (ErrGitHubOwnersComma), an entry that
// is not a GitHub login or that is the word none (ParseGitHubLogin's ErrGitHubOwnersLogin), and a
// list that names no account (ErrGitHubOwnersMissing). It answers them lowercased, sorted and
// joined by single spaces, so a Secret holding them stays comparable, two spellings of one list are
// one policy, and the value is one that both stores hold: an AWS tag value and a Secret Manager
// annotation both take a space, which a GitHub login never holds.
func ParseGitHubOwners(raw string) (string, error) {
	if strings.Contains(raw, ",") {
		return "", ErrGitHubOwnersComma
	}
	var owners []string
	for _, entry := range strings.Fields(raw) {
		owner, err := ParseGitHubLogin(entry)
		if err != nil {
			return "", err
		}
		owners = append(owners, owner)
	}
	if len(owners) == 0 {
		return "", ErrGitHubOwnersMissing
	}
	slices.Sort(owners)
	return strings.Join(slices.Compact(owners), " "), nil
}

// ErrScopesMissing is a scopes tag that names no scope: a tag that narrowed the token to nothing
// would mint a token good for no API at all, which is a mistake rather than a policy. A secret
// that wants the default mints it by carrying no scopes tag (TagScopes).
var ErrScopesMissing = errors.New("scopes names at least one OAuth scope")

// ErrScopesComma is a scopes tag that holds a comma: its names are separated by spaces, as
// github-owners' and requesters' entries are, since an AWS tag value takes a space and no comma.
var ErrScopesComma = errors.New(`scopes separates its names with spaces, not commas ("<scope> <scope>")`)

// ErrScopesEntry is a scopes entry that is not a scope name: one that does not start with a
// lowercase ASCII letter or digit, or that holds anything but lowercase ASCII letters, digits,
// '.', '_' and '-'. A whole https://www.googleapis.com/auth/… URL is the mistake a reader makes
// here, and fails the pattern on its slashes and colon; an upper-case name fails it too, rather
// than being lowercased, since a Google scope name holds no upper case at all and one written
// that way was written by hand.
var ErrScopesEntry = errors.New("scopes names the part of a Google scope after https://www.googleapis.com/auth/, in lowercase")

// ErrScopesDuplicate is a scopes tag that names one scope twice. ParseGitHubOwners quietly
// compacts a repeated login, since two spellings of one account list are one policy; a repeated
// scope is refused instead, because the value is short enough to read whole and a name written
// twice is a mistake rather than a second spelling of the same list.
var ErrScopesDuplicate = errors.New("scopes names each scope once")

// googleScopeName is what the part of a Google OAuth scope after googleScopePrefix can be: a
// lowercase ASCII letter or digit, then lowercase ASCII letters, digits, '.', '_' and '-'
// ("cloud-platform", "devstorage.read_only", "bigquery"). It is matched against the entry as
// written, so neither a full scope URL (its slashes and colon) nor an upper-case name passes.
var googleScopeName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// googleScopePrefix opens every Google OAuth scope a scopes tag names (Secret.ScopeURLs puts it
// back before the minter asks Google for the token). The tag holds the part after it alone so
// that a list of five scopes still fits an AWS tag value's 256 characters (TagScopes).
const googleScopePrefix = "https://www.googleapis.com/auth/"

// ParseScopes reads a scopes tag: the names of the OAuth scopes a google-service-account secret's
// token carries, separated by white space, each the part of a Google scope that follows
// googleScopePrefix. It refuses a comma anywhere (ErrScopesComma), a list that names no scope
// (ErrScopesMissing), an entry that is not a scope name (ErrScopesEntry, which a whole scope URL
// and an upper-case name each land on), and a name given twice (ErrScopesDuplicate). It answers
// them sorted and joined by single spaces, so a Secret holding them stays comparable, two
// spellings of one list are one policy, and the value is one that both stores hold: an AWS tag
// value and a Secret Manager annotation both take a space, which a scope name never holds.
func ParseScopes(raw string) (string, error) {
	if strings.Contains(raw, ",") {
		return "", ErrScopesComma
	}
	var scopes []string
	for _, entry := range strings.Fields(raw) {
		if !googleScopeName.MatchString(entry) {
			return "", fmt.Errorf("%w, not %q", ErrScopesEntry, entry)
		}
		if slices.Contains(scopes, entry) {
			return "", fmt.Errorf("%w, not %q twice", ErrScopesDuplicate, entry)
		}
		scopes = append(scopes, entry)
	}
	if len(scopes) == 0 {
		return "", ErrScopesMissing
	}
	slices.Sort(scopes)
	return strings.Join(scopes, " "), nil
}

// ErrRequestersMissing is a requesters tag that names nobody: a tag that let no one ask would
// serve the secret to no session at all, which is a mistake rather than a policy.
var ErrRequestersMissing = errors.New("requesters names at least one person or service")

// ErrRequestersComma is a requesters tag that holds a comma: its entries are separated by spaces,
// as github-owners' and BROKER_SERVICES' are, since an AWS tag value takes a space and no comma.
var ErrRequestersComma = errors.New(`requesters separates its entries with spaces, not commas ("<email> <service>")`)

// ErrRequestersEntry is a requesters entry that names neither a person nor a service: one holding
// an @ that is not a lowercase email of the form a person signs in under (an uppercase one is
// refused rather than lowercased, since the tag is compared against the sign-in as it is written),
// or one naming a service the deployment does not register.
var ErrRequestersEntry = errors.New("requesters names people by their lowercase sign-in email and services by their registered name")

// ParseRequesters reads a requesters tag for a caller that holds the deployment's registered
// service list — the loader (Loader.Services), the one reader whose answer decides whether the
// secret is served. People are named by lowercase email and services by name, separated by white
// space. It refuses a comma anywhere (ErrRequestersComma), a list that names nobody
// (ErrRequestersMissing), and an entry that is neither (ErrRequestersEntry). An entry holding an
// @ is a person, and must be the lowercase email their sign-in names them by (emailPattern),
// since Evaluate compares it against record.CanonicalLogin of the session's operator; any other
// entry is a service, and must be one of services, ALWAYS, so a nil or empty list refuses every
// service entry, exactly as the owner tag already refuses a name the deployment does not
// register (Loader.secret). ParseRequesterShapes checks a service entry's shape alone, for the
// one caller that holds no registered list. It answers the entries sorted, each
// once and joined by single spaces, so a Secret holding them stays comparable, two spellings of
// one list are one policy, and the value is one an AWS tag and a Secret Manager annotation both
// hold.
func ParseRequesters(raw string, services []string) (string, error) {
	return parseRequesters(raw, services, true)
}

// ParseRequesterShapes reads a requesters tag for a caller that holds no registered service list:
// `agent-secrets`, which never sees BROKER_SERVICES. It is ParseRequesters in every respect but
// one — an entry naming a service is checked for a service name's shape alone
// (record.ValidService) rather than against the registered list — so the CLI refuses a malformed
// tag before it writes, and the broker's answer to the write's reread (ReasonRequestersMalformed)
// is what catches a name no deployment registers. ParseRequesters exists for the loader, which
// holds that list and must apply it.
func ParseRequesterShapes(raw string) (string, error) {
	return parseRequesters(raw, nil, false)
}

// parseRequesters is the one reading behind both exported functions. registered says which check
// a service entry gets: membership of services, or a service name's shape alone.
func parseRequesters(raw string, services []string, registered bool) (string, error) {
	if strings.Contains(raw, ",") {
		return "", ErrRequestersComma
	}
	var requesters []string
	for _, entry := range strings.Fields(raw) {
		switch {
		case strings.Contains(entry, "@"):
			if !emailPattern.MatchString(entry) {
				return "", fmt.Errorf("%w, not %q", ErrRequestersEntry, entry)
			}
		case registered:
			if !slices.Contains(services, entry) {
				return "", fmt.Errorf("%w, not %q: no registered service has that name", ErrRequestersEntry, entry)
			}
		default:
			if !record.ValidService(entry) {
				return "", fmt.Errorf("%w, not %q", ErrRequestersEntry, entry)
			}
		}
		requesters = append(requesters, entry)
	}
	if len(requesters) == 0 {
		return "", ErrRequestersMissing
	}
	slices.Sort(requesters)
	return strings.Join(slices.Compact(requesters), " "), nil
}

// SplitRequesters splits a requesters tag into the people it names and the services it names,
// each list in the order the tag holds them and both nil for a tag that names nobody. The @ is
// the whole of the test: both parsers behind the tag make every person entry a lowercase email,
// which holds one, and every service entry a service name, which holds none (ParseRequesters
// against the deployment's registered list, ParseRequesterShapes against a service name's shape
// alone), so a reader of the split needs neither alphabet and no entry can be both. An entry is
// answered as it was written, since a tag the loader read is already sorted, compacted and
// single-spaced and a caller comparing an entry against an owner tag compares it as the sign-in
// writes it.
func SplitRequesters(requesters string) (people, services []string) {
	for _, entry := range strings.Fields(requesters) {
		if strings.Contains(entry, "@") {
			people = append(people, entry)
		} else {
			services = append(services, entry)
		}
	}
	return people, services
}

// devServiceAccount is the id, the part before the @, of a service account the deployment binds
// to one person: it binds each person's DEV_<NAME> secret to their own dev-<name> account, whose
// token therefore goes to that person and to nobody else. RequesterRule reads it to refuse a
// service entry on such a secret's requesters list.
var devServiceAccount = regexp.MustCompile(`^dev-[a-z0-9-]+$`)

// TagSet is one secret's tags as TagRules weighs them: the values the loader read from the store
// (Loader.secret) or the values `agent-secrets secret create` and `retag` will write, beside the
// one thing that is no tag at all, BoundAccount.
type TagSet struct {
	// Kind is the secret's kind tag, "" for a stored secret; Owner its owner tag; Tier its tier
	// tag; Requesters its requesters tag as ParseRequesters or ParseRequesterShapes answered it,
	// "" for a secret that lists nobody; Scopes its scopes tag as ParseScopes answered it, "" for
	// a secret that names none.
	Kind, Owner, Tier, Requesters, Scopes string
	// BoundAccount is the account email the deployment's BROKER_SERVICE_ACCOUNT_SECRETS binds to
	// this secret, and "" when it binds none. It is "" as well for a caller that does not hold
	// the setting — `agent-secrets`, which never sees it, exactly as it never sees BROKER_SERVICES
	// and so calls ParseRequesterShapes rather than ParseRequesters — so the rule it feeds, the
	// one refusing a service entry on a secret bound to a dev-<name> account, is the broker's to
	// apply on the write's reread.
	BoundAccount string
	// Admin is true when the secret carries admin: "true" (TagAdmin).
	Admin bool
	// MaxGrant is the secret's max-grant tag as ParseMaxGrant answered it, and 0 when it sets
	// none.
	MaxGrant time.Duration
}

// RequesterFault is which condition of the requesters rules refuses a secret's requesters tag,
// as RequesterRule answers it. TagRules turns every fault but RequesterFaultNone into the one
// load reason ReasonRequestersNotOwner, so the fault is what tells those conditions apart for a
// caller that must say which flag fixes a write.
type RequesterFault int

const (
	// RequesterFaultNone is a tag set no requesters condition refuses.
	RequesterFaultNone RequesterFault = iota
	// RequesterFaultOwnerNotPerson is a google-service-account secret whose owner is no person
	// (ValidPersonOwner). The token it mints acts as a service account, which is given to the
	// one person who owns the secret, so a secret owned by shared or by a service can carry no
	// such kind and no requesters list could make it servable.
	RequesterFaultOwnerNotPerson
	// RequesterFaultServiceBelowHumanTier is a google-service-account secret whose requesters
	// name a service at a tier that is not human. A service's pod may ask for such a token only
	// where a person approves each release of it, which human tier is.
	RequesterFaultServiceBelowHumanTier
	// RequesterFaultNotTheOwnerOnce is a google-service-account secret whose requesters do not
	// name its person owner exactly once: no list at all, a list naming no person, one naming
	// somebody else, or one naming more than one person. The secret is served to that one
	// person, who is therefore on its list, beside the registered services a human-tier secret
	// may name as well.
	RequesterFaultNotTheOwnerOnce
	// RequesterFaultServiceOnDevAccount is a secret the deployment binds to a dev-<name> account
	// (TagSet.BoundAccount, devServiceAccount) whose requesters name a service, at either tier.
	// Such an account is one person's own, so no service may ask for its token, whatever kind
	// the secret is.
	RequesterFaultServiceOnDevAccount
)

// RequesterRule answers which condition refuses t's requesters, and RequesterFaultNone for a set
// no such condition refuses. It holds the conditions most fundamental first: the owner is no
// person, so no list could make the secret servable; the list names a service below human tier;
// the list does not name that owner exactly once; the secret is bound to a dev-<name> account
// and the list names a service, which holds for every kind.
//
// The loader reads none of these apart: it reads TagRules' reason, and every fault here is the
// one reason ReasonRequestersNotOwner. The fault exists for `agent-secrets`' tagRefusal, which
// says which flag fixes the write and so must name the condition that refused it rather than
// another that happens to be true beside it — a secret owned by shared is answered for its owner
// however its list and tier read, and a list naming a service at agent tier is answered for the
// tier and not for the person it leaves out.
func RequesterRule(t TagSet) RequesterFault {
	// Each entry's own shape is ParseRequesters' and ParseRequesterShapes' business, and they
	// answer the entries sorted and separated by single spaces, so the conditions below read
	// only the @ that tells a person from a service (SplitRequesters), and read it once for all
	// of them.
	people, services := SplitRequesters(t.Requesters)
	if t.Kind == KindGoogleServiceAccount {
		switch {
		case !ValidPersonOwner(t.Owner):
			return RequesterFaultOwnerNotPerson
		case len(services) > 0 && t.Tier != TierHuman:
			return RequesterFaultServiceBelowHumanTier
		case len(people) != 1 || people[0] != t.Owner:
			return RequesterFaultNotTheOwnerOnce
		}
	}
	if id, _, bound := strings.Cut(t.BoundAccount, "@"); bound && devServiceAccount.MatchString(id) && len(services) > 0 {
		return RequesterFaultServiceOnDevAccount
	}
	return RequesterFaultNone
}

// TagRules is the one statement of what a secret's tags demand of each other, which the loader
// applies to every secret it reads and `agent-secrets secret` create and retag apply before they
// write (so it asks for no registered service list of its own). It takes a TagSet rather than a
// positional list because the rules weigh eight values — the kind, owner, tier, requesters,
// scopes, admin and max-grant tags, and the deployment's binding of the secret to an account —
// which no caller could pass in order without mistaking one for another. It answers "" for a
// secret the rules allow, and otherwise the load reason that refuses it:
//
//   - ReasonScopesKind: a secret that is not a google-service-account one carries a scopes tag.
//     No other minter reads it, so the tag would narrow nothing and say that it had.
//   - ReasonAdminTier: an admin secret is not human tier. An administrator's credential is
//     released only by a person's approval, which agent tier does not ask for.
//   - ReasonAdminRequesters: an admin secret carries no requesters tag. Who may ask for an
//     administrator's credential is named, never left to owner and tier alone.
//   - ReasonMaxGrantRequired: an admin secret, or a google-service-account one, sets no
//     max-grant or one longer than an hour. Such a grant is reviewed while it is live, so it
//     lasts an hour at most. A secret that merely lists its requesters keeps its own cap, or
//     none.
//   - ReasonRequestersNotOwner: a condition of RequesterRule refuses the secret's requesters —
//     a google-service-account secret whose owner is no person, one whose list names a service
//     below human tier, or one whose list does not name its person owner exactly once, and a
//     secret of any kind the deployment binds to a dev-<name> account whose list names a
//     service. The one reason is all a load answers; which of those conditions refused it is
//     RequesterRule's fault, which `agent-secrets` reads to name the flag that fixes the write.
//
// The rules above are weighed in the order they are written, so a secret several of them refuse
// is refused by the first.
func TagRules(t TagSet) string {
	if t.Scopes != "" && t.Kind != KindGoogleServiceAccount {
		return ReasonScopesKind
	}
	if t.Admin && t.Tier != TierHuman {
		return ReasonAdminTier
	}
	if t.Admin && t.Requesters == "" {
		return ReasonAdminRequesters
	}
	if (t.Admin || t.Kind == KindGoogleServiceAccount) && (t.MaxGrant == 0 || t.MaxGrant > time.Hour) {
		return ReasonMaxGrantRequired
	}
	if RequesterRule(t) != RequesterFaultNone {
		return ReasonRequestersNotOwner
	}
	return ""
}

// gitHubRepositoryName is what a repository name GitHub creates can be: ASCII letters, digits,
// '.', '-' and '_', at most 100 of them. The names . and .. are refused beside it.
var gitHubRepositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// NarrowingError is ParseGitHubNarrowing's refusal: Field is the narrowing's field it names,
// "owner" or "repositories", so a caller can name it in its own terms (the API's
// github_app.<field>, the CLI's flag), and Reason says what is wrong with it.
type NarrowingError struct {
	Field, Reason string
}

func (e *NarrowingError) Error() string {
	return e.Field + " " + e.Reason
}

// ParseGitHubNarrowing checks one release's narrowing of its GitHub App tokens as a caller wrote it
// and answers it in its canonical form: owner one GitHub login (ParseGitHubLogin, once white space
// around it is trimmed), lowercased, and repositories names GitHub can hold, without the owner, at
// most secrets.MaxNarrowedRepositories, sorted and each once. Repositories nil names none, so the
// token reaches every repository of the installation; present but empty is refused, since a caller
// that built it from a list that matched nothing would otherwise get that widest token. Nothing
// else reaches GitHub: the permissions a narrowing can name are fixed, so a caller can only
// narrow. It answers its refusal as a *NarrowingError, typed rather than error, so a caller names
// the field without reading the text. The broker applies a narrowing in this form and names it back in
// the release's answer, and the CLI builds its request with this function too, so the CLI's check
// that the broker applied the narrowing it asked for compares like with like.
func ParseGitHubNarrowing(owner string, repositories []string, readOnly bool) (secrets.GitHubNarrowing, *NarrowingError) {
	login, err := ParseGitHubLogin(strings.TrimSpace(owner))
	if err != nil {
		return secrets.GitHubNarrowing{}, &NarrowingError{"owner", fmt.Sprintf("must name one GitHub login, not %q", owner)}
	}
	if repositories != nil && len(repositories) == 0 {
		return secrets.GitHubNarrowing{}, &NarrowingError{"repositories", "is empty: name at least one repository, or leave it out for every repository of the installation"}
	}
	if len(repositories) > secrets.MaxNarrowedRepositories {
		return secrets.GitHubNarrowing{}, &NarrowingError{"repositories", fmt.Sprintf("names %d repositories; the broker takes at most %d", len(repositories), secrets.MaxNarrowedRepositories)}
	}
	for _, name := range repositories {
		if !gitHubRepositoryName.MatchString(name) || name == "." || name == ".." {
			return secrets.GitHubNarrowing{}, &NarrowingError{"repositories", fmt.Sprintf("names %s's repositories by name alone, in ASCII letters, digits, '.', '-' and '_', not %q", login, name)}
		}
	}
	sorted := slices.Clone(repositories)
	slices.Sort(sorted)
	return secrets.GitHubNarrowing{Owner: login, Repositories: slices.Compact(sorted), ReadOnly: readOnly}, nil
}

// KindNeedsGitHubOwners reports whether a secret of kind must carry a github-owners tag naming at
// least one account. Only a github-app secret reads the tag.
func KindNeedsGitHubOwners(kind string) bool {
	return kind == KindGitHubApp
}

// GitHubOwnersFor is the github-owners rule every reader and writer of a secret's tags applies:
// the loader, and `agent-secrets secret create` and `retag` before they write. For a kind that
// needs owners (KindNeedsGitHubOwners) it answers ParseGitHubOwners of raw, with its two errors;
// for any other kind the tag is not read, and it answers "".
func GitHubOwnersFor(kind, raw string) (string, error) {
	if !KindNeedsGitHubOwners(kind) {
		return "", nil
	}
	return ParseGitHubOwners(raw)
}

// KindSource is the Source of a kind secret: its kind frozen beside the store's own source.
// A store's source never starts with kindSourcePrefix (an AWS source starts with "arn:", a
// Secret Manager one with "projects/").
func KindSource(kind, storeSource string) string {
	return kindSourcePrefix + kind + ":" + storeSource
}

// ErrKindSourceMalformed is a source that opens with kindSourcePrefix but names no kind or no
// store source, which no KindSource built.
var ErrKindSourceMalformed = errors.New("a minted source names no kind or no store source")

// ParseKindSource splits a source KindSource built into its kind and store source. minted is false
// for a stored secret's source, which it answers whole as storeSource.
func ParseKindSource(source string) (kind, storeSource string, minted bool, err error) {
	rest, minted := strings.CutPrefix(source, kindSourcePrefix)
	if !minted {
		return "", source, false, nil
	}
	kind, storeSource, ok := strings.Cut(rest, ":")
	if !ok || kind == "" || storeSource == "" {
		return "", "", true, ErrKindSourceMalformed
	}
	return kind, storeSource, true, nil
}

// ErrUnknownSecret is a name no secret the policy serves carries: none exists under the namespace
// prefix, or the one that does was refused.
var ErrUnknownSecret = errors.New("no agent secret has this name")

// ReasonAbsent is LoadOne's answer for a name no secret carries: none exists under the prefix, or
// the one that does is scheduled for deletion. It is not a refusal, so it is never logged as
// RefusedMessage.
const ReasonAbsent = "absent"

type ownerKind int

const (
	ownerPerson ownerKind = iota
	ownerShared
	ownerService
)

// Secret is one namespace secret the policy serves.
type Secret struct {
	// Name is what a session asks for: the secret's name under the prefix, uppercased, each hyphen
	// an underscore (<prefix>deel-api-key is DEEL_API_KEY).
	Name string
	// Source is what the secret's value is read by: its ARN in Secrets Manager, or its resource
	// name in Secret Manager. A kind secret's is KindSource of its kind and that store source, so
	// the kind is frozen into every request (ParseKindSource reads it back).
	Source string
	// Owner is OwnerShared, a person's email, or a registered service.
	Owner string
	// Tier is TierAgent or TierHuman.
	Tier string
	// MaxGrant is the greatest grant lifetime a request may sign for this secret, or 0 when it sets
	// none (ParseMaxGrant refuses anything under a second). A value, never a pointer, so a Secret
	// compares equal to an unchanged reread of itself (Current.RefreshOne).
	MaxGrant time.Duration
	// GitHubOwners is a github-app secret's github-owners tag as ParseGitHubOwners answers it, and
	// "" for every other secret. A string, not a slice, for the same reason as MaxGrant.
	GitHubOwners string
	// Requesters is the secret's requesters tag as ParseRequesters answers it, and "" for a secret
	// that carries none, which every session and pod may ask for. A string, not a slice, for the
	// same reason as MaxGrant.
	Requesters string
	// Scopes is a google-service-account secret's scopes tag as ParseScopes answers it, and "" for
	// a secret that carries none, whose token the minter asks for with the cloud-platform scope
	// alone (TagScopes, Secret.ScopeURLs). A string, not a slice, for the same reason as MaxGrant.
	Scopes string
	// Admin is true when the secret carries admin: "true" (TagAdmin): its grant is an
	// administrator's, which the rules hold to human tier, a requesters list and an hour's cap
	// (TagRules).
	Admin bool
	// SessionDefault is true when the secret's default-grant tag is "session": a request naming
	// no lifetime gets a grant that lasts until its session ends instead of DefaultGrantLifetime.
	SessionDefault bool
	ownership      ownerKind
}

// Kind is the kind the secret is served as, read back from its Source (KindSource), or "" for a
// stored secret.
func (secret Secret) Kind() string {
	kind, _, minted, err := ParseKindSource(secret.Source)
	if !minted || err != nil {
		return ""
	}
	return kind
}

// GitHubOwnerLogins is GitHubOwners as a list of lowercase logins, nil when it is "".
func (secret Secret) GitHubOwnerLogins() []string {
	if secret.GitHubOwners == "" {
		return nil
	}
	return strings.Fields(secret.GitHubOwners)
}

// ScopeURLs is Scopes as the list of whole Google OAuth scope URLs the minter asks Google for,
// each name under googleScopePrefix, and nil when it is "" — a secret that names none, for which
// the minter chooses its own default rather than reading an empty list as no scope at all.
func (secret Secret) ScopeURLs() []string {
	if secret.Scopes == "" {
		return nil
	}
	names := strings.Fields(secret.Scopes)
	urls := make([]string, 0, len(names))
	for _, name := range names {
		urls = append(urls, googleScopePrefix+name)
	}
	return urls
}

// requesterNames is Requesters as a list of the emails and service names it holds, nil when it
// is "". It is unexported because lists, just below, is its one caller: nothing outside this
// package reads a secret's requesters as a list.
func (secret Secret) requesterNames() []string {
	if secret.Requesters == "" {
		return nil
	}
	return strings.Fields(secret.Requesters)
}

// lists reports whether secret's requesters name r: the person whose session it is, or the
// service of the pod it is. Each sort is matched only by an entry of its own sort, which the @
// tells apart (ParseRequesters makes every person entry hold one and every service entry hold
// none): a login record.CanonicalLogin answers may be a GitHub login rather than an email, for
// a record decided before people were named by email, and such a login has a service name's own
// alphabet, so an operator named "legion-daemon" would otherwise be served every secret that
// lists the legion-daemon service. An empty Operator (a pod has no person) and an empty Service
// (a session has no service) match nothing, never an entry.
func (secret Secret) lists(r Requester) bool {
	operator := record.CanonicalLogin(r.Operator)
	for _, entry := range secret.requesterNames() {
		person := strings.Contains(entry, "@")
		if person && operator != "" && entry == operator {
			return true
		}
		if !person && r.Service != "" && entry == r.Service {
			return true
		}
	}
	return false
}

// approver is who decides an approval request for secret: anyone signed in to Dispatch for a
// shared secret, its owner otherwise.
func (secret Secret) approver() string {
	if secret.ownership == ownerShared {
		return record.AnyoneApprover
	}
	return secret.Owner
}

// Set is the policy as one load read it, or as a single name's reread left it (NewSet builds both).
type Set struct {
	// Version is the SHA-256 of every served secret's name, owner, tier, source, max-grant,
	// github-owners, requesters, scopes and admin: equal versions are equal policies. A request
	// records the version it was decided under, so a live grant is re-evaluated only once the
	// policy changed.
	Version string
	Secrets map[string]Secret
}

// NewSet builds the one Set shape both Load and the single-name merges produce, keeping secrets
// (keyed by Name) as its Secrets. A secret with no max-grant, no github-owners, no requesters, no
// scopes and no admin tag writes the pre-kind digest bytes,
// "<Name>\t<Owner>\t<Tier>\t<Source>\n" (a kind secret's kind is in its Source); one with any of
// them adds the first two, as a fifth field (time.Duration's form, 0s for none) and a sixth
// (empty for none), and then ends its line with "\trequesters=<list>", "\tscopes=<list>" and
// "\tadmin=true", in that order, for each of those three it carries. Entries are ascending by the
// secret's name in the store (the slug) — the exact order Load wrote before NewSet existed, so
// deployed plain-secret digests do not shift. (Sorting by the uppercased Name would NOT be
// equivalent: '-' is 0x2D, '_' is 0x5F, so "a-b" < "a0" as slugs but "A0" < "A_B" as Names.)
func NewSet(secrets map[string]Secret) *Set {
	type bySlug struct {
		slug   string
		secret Secret
	}
	served := make([]bySlug, 0, len(secrets))
	for _, s := range secrets {
		served = append(served, bySlug{slug: slugOf(s.Name), secret: s})
	}
	slices.SortFunc(served, func(a, b bySlug) int { return strings.Compare(a.slug, b.slug) })
	digest := sha256.New()
	for _, s := range served {
		requesters := ""
		if s.secret.Requesters != "" {
			requesters = "\trequesters=" + s.secret.Requesters
		}
		scopes := ""
		if s.secret.Scopes != "" {
			scopes = "\tscopes=" + s.secret.Scopes
		}
		admin := ""
		if s.secret.Admin {
			admin = "\tadmin=true"
		}
		if s.secret.MaxGrant == 0 && s.secret.GitHubOwners == "" && !s.secret.SessionDefault && s.secret.Requesters == "" && s.secret.Scopes == "" && !s.secret.Admin {
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source)
			continue
		}
		if s.secret.SessionDefault {
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\t%s\t%s\tdefault-grant=session%s%s%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source, s.secret.MaxGrant, s.secret.GitHubOwners, requesters, scopes, admin)
			continue
		}
		fmt.Fprintf(digest, "%s\t%s\t%s\t%s\t%s\t%s%s%s%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source, s.secret.MaxGrant, s.secret.GitHubOwners, requesters, scopes, admin)
	}
	return &Set{Version: hex.EncodeToString(digest.Sum(nil)), Secrets: secrets}
}

// Requester is an enrolled session or pod as the policy sees it.
type Requester struct {
	// Operator is the email of the person whose machine login the session enrolled under; empty
	// for a pod.
	Operator string
	// Service is the service of the machine login that enrolled the session, set only when the
	// session is a pod whose verified service account is one of the accounts BROKER_SERVICES maps
	// to that same service (requests.Machine.requester); empty otherwise. A service may list
	// several accounts, but each account is listed once and proves exactly one service, since a
	// machine login's service is its own claim and only the pod's account can prove it: an
	// account listed for another service, or for none, proves nothing here.
	Service string
	// Withheld is the names the session's operator withheld from it by revoking a grant of them
	// the session got without asking (requests.Machine.RevokeByApprover). Evaluate answers each as
	// a human-tier secret.
	Withheld []string
}

// Decision is how the policy answers one requester's ask for one name.
type Decision struct {
	// Outcome is Automatic, Approval or Deny.
	Outcome string
	// Approver is who decides an Approval: the owner's email, or record.AnyoneApprover for a
	// shared secret.
	Approver string
	// Source is what the secret's value is read by (Secret.Source).
	Source string
	// Reason names the rule behind a Deny the policy can name, and is empty for every other
	// decision: a caller tells a session why it was refused without reading the policy itself.
	Reason string
}

// ServiceOwned reports whether the set serves name as a registered service's secret. Who is a
// service's own session rests on BROKER_SERVICES' accounts, which are not in Version, so a caller
// re-checks a grant of such a name on every use rather than only once Version moves.
func (s *Set) ServiceOwned(name string) bool {
	return s.Secrets[name].ownership == ownerService
}

// ReasonRequesterNotListed is the Reason of a Deny the requesters tag decided: neither the
// person the asking session is signed in as nor the service the asking pod proves is on the
// secret's list, so it is refused whatever its owner and tier would have given them.
const ReasonRequesterNotListed = "requester-not-listed"

// Evaluate answers r's ask for name. A name the set does not serve is ErrUnknownSecret. A secret
// whose requesters are listed (Secret.Requesters) is denied, ReasonRequesterNotListed, to every
// requester the list does not name, before owner and tier are read at all. A name withheld from r
// (Requester.Withheld) is decided as a human-tier secret: what r would have got at once is an
// approval request to the owner, or to anyone for a shared secret, and a service's own secret,
// which no person approves, is denied.
func (s *Set) Evaluate(name string, r Requester) (Decision, error) {
	secret, ok := s.Secrets[name]
	if !ok {
		return Decision{}, ErrUnknownSecret
	}
	if secret.Requesters != "" && !secret.lists(r) {
		return Decision{Outcome: Deny, Reason: ReasonRequesterNotListed, Source: secret.Source}, nil
	}
	withheld := slices.Contains(r.Withheld, name)
	d := Decision{Source: secret.Source}
	switch {
	case secret.ownership == ownerService:
		d.Outcome = Deny
		if r.Service == secret.Owner && !withheld {
			d.Outcome = Automatic
		}
	case secret.Tier == TierAgent && !withheld && (secret.ownership == ownerShared || record.CanonicalLogin(r.Operator) == secret.Owner):
		d.Outcome = Automatic
	default:
		d.Outcome, d.Approver = Approval, secret.approver()
	}
	return d, nil
}
