// packages/envoy/cmd/agent-secrets/secret_tags.go
//
// The secret forms' tag rules: what each tag flag accepts, under the broker's own rules, and the
// tags a write sends.
package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// checkOwnerTier refuses an --owner that is neither me nor shared and a --tier that is neither
// agent nor human; "" passes either, for a form where it is optional.
func checkOwnerTier(owner, tier string) error {
	var errs []error
	if owner != "" && owner != "me" && owner != policy.OwnerShared {
		errs = append(errs, fmt.Errorf("--owner is me or %s, not %q", policy.OwnerShared, owner))
	}
	if tier != "" && tier != policy.TierAgent && tier != policy.TierHuman {
		errs = append(errs, fmt.Errorf("--tier is %s or %s, not %q", policy.TierAgent, policy.TierHuman, tier))
	}
	if len(errs) > 0 {
		return usageErr{errors.Join(errs...)}
	}
	return nil
}

// tagNone is the value of --kind, --github-owners, --requesters, --scopes, --admin, --max-grant
// and --default-grant that removes the tag, the one the broker refuses as a github-owners login
// (policy.TagRemoved).
const tagNone = policy.TagRemoved

// checkKind is --kind's rule: a kind the broker mints (policy.ValidKind), or none.
func checkKind(kind string) error {
	if kind == "" || kind == tagNone || policy.ValidKind(kind) {
		return nil
	}
	return usageErr{fmt.Errorf("--kind is %s or %s, not %q", strings.Join(policy.Kinds, ", "), tagNone, kind)}
}

// checkMaxGrant is --max-grant's rule, the broker's own (policy.ParseMaxGrant), or none where
// orNone allows it.
func checkMaxGrant(maxGrant string, orNone bool) error {
	if maxGrant == "" || (orNone && maxGrant == tagNone) {
		return nil
	}
	if _, err := policy.ParseMaxGrant(maxGrant); err != nil {
		return usageErr{fmt.Errorf("--max-grant: %w", err)}
	}
	return nil
}

// checkGitHubOwners is --github-owners' rule, the broker's own (policy.ParseGitHubOwners): GitHub
// account logins separated by spaces, which the shell quotes as one argument; a comma is refused.
// none is a sentinel, not a login: alone, and only where orNone allows it (retag), it removes the
// tag; given on create, or beside a login, or in another case, it is refused, since it would
// otherwise be written as a login or dropped as the removal (optionalTags), leaving a github-app
// secret the broker refuses. It answers the list as the broker reads it, lowercased, sorted and
// single-spaced, so the tag is written as it will be served.
func checkGitHubOwners(owners string, orNone bool) (string, error) {
	if owners == "" || (orNone && owners == tagNone) {
		return owners, nil
	}
	for _, field := range strings.Fields(owners) {
		if !strings.EqualFold(field, tagNone) {
			continue
		}
		if orNone {
			return "", usageErr{fmt.Errorf("--github-owners %q: none removes the tag and is given alone, as --github-owners none; a GitHub login list names no none", owners)}
		}
		return "", usageErr{fmt.Errorf("--github-owners %q: none names no GitHub account, and removes the tag only on retag; a github-app secret needs at least one login", owners)}
	}
	parsed, err := policy.ParseGitHubOwners(owners)
	if err != nil {
		return "", usageErr{fmt.Errorf("--github-owners: %w", err)}
	}
	return parsed, nil
}

// checkGitHubOwnersFor applies the broker's github-owners rule (policy.GitHubOwnersFor) to the
// kind and owners a secret will hold once a write lands, so create and retag refuse, before any
// write, a github-app secret the broker would then refuse as github-owners-missing or
// github-owners-malformed, each named as the broker names it. named says the command itself named
// --github-owners: owners named for a kind that does not read them are refused too, since the
// broker would ignore them, while owners the secret already holds are left alone whatever its kind
// becomes.
func checkGitHubOwnersFor(kind, owners string, named bool) error {
	if !policy.KindNeedsGitHubOwners(kind) {
		if named {
			return usageErr{fmt.Errorf("--github-owners is read only on a %s secret, and this one's kind is %q", policy.KindGitHubApp, kind)}
		}
		return nil
	}
	_, err := policy.GitHubOwnersFor(kind, owners)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, policy.ErrGitHubOwnersMissing):
		return usageErr{fmt.Errorf("a %s secret needs --github-owners naming at least one GitHub account, or the broker refuses it as %s", kind, policy.ReasonGitHubOwnersMissing)}
	default:
		return usageErr{fmt.Errorf("the secret's github-owners tag %q is malformed (%w), and the broker refuses it as %s: name the accounts with --github-owners", owners, err, policy.ReasonGitHubOwnersMalformed)}
	}
}

// checkRequesters is --requesters' rule, the broker's own (policy.ParseRequesterShapes): people
// by their lowercase sign-in email and services by their registered name, separated by spaces,
// which the shell quotes as one argument; a comma is refused. This CLI checks shapes rather than
// the registered list, since it holds none: an entry naming a service is checked for a service
// name's shape alone here, and a name the deployment does not register is caught by the broker's
// answer to the reread that follows the write. none is a sentinel, not a requester: alone, and
// only where orNone allows it (retag), it removes the tag; given on create, or beside a
// requester, it is refused, since it would otherwise be written as a service named none or
// dropped as the removal (optionalTags), leaving a secret nobody but none may ask for. It
// answers the list as the broker reads it, sorted, each entry once and single-spaced, so the tag
// is written as it will be served.
func checkRequesters(list string, orNone bool) (string, error) {
	if list == "" || (orNone && list == tagNone) {
		return list, nil
	}
	for _, field := range strings.Fields(list) {
		if !strings.EqualFold(field, tagNone) {
			continue
		}
		if orNone {
			return "", usageErr{fmt.Errorf("--requesters %q: none removes the tag and is given alone, as --requesters none; a requesters list names no none", list)}
		}
		return "", usageErr{fmt.Errorf("--requesters %q: none names no person or service, and removes the tag only on retag; leave --requesters off to let every session the owner and tier allow ask", list)}
	}
	parsed, err := policy.ParseRequesterShapes(list)
	if err != nil {
		return "", usageErr{fmt.Errorf("--requesters: %w", err)}
	}
	return parsed, nil
}

// adminTrue is the one value the admin tag holds, the policy's own AdminTrue: anything else, the
// empty string included, the broker refuses as policy.ReasonAdminMalformed, so --admin takes this
// one word, checkAdmin refuses the rest before any write, and checkTagsFor reads the mark off the
// tag by comparing against it. Aliased rather than restated, as tagNone aliases policy.TagRemoved,
// so this CLI cannot drift from the loader on what the tag says.
const adminTrue = policy.AdminTrue

// checkScopes is --scopes' rule, the broker's own (policy.ParseScopes): the names of the OAuth
// scopes a google-service-account secret's token carries, each the part of a Google scope that
// follows https://www.googleapis.com/auth/, separated by spaces, which the shell quotes as one
// argument; a comma, a whole scope URL, an upper-case name, a name given twice and a list naming
// no scope at all are each refused. none is a sentinel, not a scope name: alone, and only where
// orNone allows it (retag), it removes the tag; given on create, or beside a scope, it is
// refused, since "none" passes the broker's own pattern for a scope name and would otherwise be
// written as a scope by that name, or dropped as the removal (optionalTags), leaving a secret
// minting a token good for no API. It answers the list as the broker reads it, sorted and
// single-spaced, so the tag is written as it will be served.
func checkScopes(scopes string, orNone bool) (string, error) {
	if scopes == "" || (orNone && scopes == tagNone) {
		return scopes, nil
	}
	for _, field := range strings.Fields(scopes) {
		if !strings.EqualFold(field, tagNone) {
			continue
		}
		if orNone {
			return "", usageErr{fmt.Errorf("--scopes %q: none removes the tag and is given alone, as --scopes none; a scope list names no none", scopes)}
		}
		return "", usageErr{fmt.Errorf("--scopes %q: none names no OAuth scope, and removes the tag only on retag; leave --scopes off to mint the cloud-platform scope alone", scopes)}
	}
	parsed, err := policy.ParseScopes(scopes)
	if err != nil {
		return "", usageErr{fmt.Errorf("--scopes: %w", err)}
	}
	return parsed, nil
}

// checkAdmin is --admin's rule: adminTrue, the one value the broker reads (policy.TagAdmin), or
// none where orNone allows it (retag) to remove the tag. Any other value is refused here, before
// anything is read or written, as the broker refuses it on the reread, policy.ReasonAdminMalformed,
// named as the broker names it: the mark says an administrator's credential is behind the secret,
// and a value saying anything else is a mistake rather than a secret that is no administrator's.
func checkAdmin(admin string, orNone bool) error {
	if admin == "" || admin == adminTrue || (orNone && admin == tagNone) {
		return nil
	}
	if orNone {
		return usageErr{fmt.Errorf("--admin is %s or %s, not %q, or the broker refuses the secret as %s", adminTrue, tagNone, admin, policy.ReasonAdminMalformed)}
	}
	return usageErr{fmt.Errorf("--admin is %s, not %q, or the broker refuses the secret as %s; leave --admin off for a secret whose grant is no administrator's", adminTrue, admin, policy.ReasonAdminMalformed)}
}

// checkTagsFor applies the broker's cross-tag rules (policy.TagRules) to the tags a secret will
// hold once a write lands — its kind, owner, tier, requesters, scopes, admin mark and max grant
// — so create (cmdSecretCreate) and retag (cmdSecretRetag) refuse, before any write, a secret
// the broker would then refuse to load. It builds the one policy.TagSet those rules weigh and
// hands it, beside the reason they answered and the map it was built from, to tagRefusal, which
// says that refusal as the flag that fixes it.
//
// It reads those tags out of the map the write leaves behind — optionalTags with the owner and
// tier create will send, the after map retag computes — rather than out of the flags, because
// optionalTags has already dropped a flag left unset and a flag set to none, so the check reads
// the tags the broker will read: `--kind none` leaves a stored secret, which carries no kind at
// all and is not a secret of a kind called none. Reading the map also leaves the check and the
// write one value, so no tag can be weighed in one form and written in another.
//
// The tier is one of the tags read because two of those rules turn on it: an admin secret is
// human tier, and a google-service-account secret may list a registered service beside its
// owner at human tier alone, so a check passing no tier would refuse a write the broker
// accepts. The max-grant tag is parsed here (policy.ParseMaxGrant) rather than by a second
// --max-grant rule: a malformed one is no duration at all, which the flag's own rule
// (checkMaxGrant) refused for a command that named it, and which the broker refuses as
// max-grant-malformed before it weighs policy.TagRules at all. The admin mark is the tag being
// adminTrue; a stored admin tag holding anything else is no rule of TagRules but
// policy.ReasonAdminMalformed, which the broker answers to the reread that follows the write,
// exactly as it answers for a stored requesters or scopes tag this CLI cannot repair. The one
// field of the set left empty is BoundAccount, the deployment's BROKER_SERVICE_ACCOUNT_SECRETS
// binding, which `agent-secrets` never sees, just as it never sees BROKER_SERVICES and so calls
// ParseRequesterShapes rather than ParseRequesters: the rule it feeds, the one refusing a
// service on a secret bound to a dev-<name> account, is the broker's to apply on that reread.
func checkTagsFor(tags map[string]string) error {
	duration, _ := policy.ParseMaxGrant(tags[policy.TagMaxGrant])
	set := policy.TagSet{
		Kind:         tags[policy.TagKind],
		Owner:        tags[policy.TagOwner],
		Tier:         tags[policy.TagTier],
		Requesters:   tags[policy.TagRequesters],
		Scopes:       tags[policy.TagScopes],
		BoundAccount: "",
		Admin:        tags[policy.TagAdmin] == adminTrue,
		MaxGrant:     duration,
	}
	return tagRefusal(policy.TagRules(set), set, tags)
}

// tagRefusal says one cross-tag refusal as the flag that fixes it: reason is what
// policy.TagRules answered for set, and tags is the map set was built from. It answers nil for
// "", the answer for a secret the rules allow, and a usage error for every other reason, so it
// is this CLI's whole mirror of TagRules: an arm for policy.ReasonScopesKind,
// policy.ReasonAdminTier, policy.ReasonAdminRequesters, policy.ReasonMaxGrantRequired and
// policy.ReasonRequestersNotOwner, and one fallback refusal after the switch, which quotes the
// reason TagRules gave. That reason is this binary's own TagRules answer, so a reason, or a
// requesters fault, with no arm here is a gap in this build of agent-secrets, which the fallback
// refuses the write for rather than leave it to be written and refused on the reread
// (TestTagRefusalRefusesAReasonNoArmNames). A broker newer than this client, holding a rule this
// client's TagRules does not know, is caught by that reread alone.
//
// Every arm reads the set rather than a flag, so no message can name tags other than the ones
// the rules judged; the max-grant arm reads tags as well, to quote the cap too long as whoever
// named it wrote it. It takes the reason as an argument, rather than calling TagRules itself,
// so a test can hand it one.
func tagRefusal(reason string, set policy.TagSet, tags map[string]string) error {
	switch reason {
	case "":
		return nil
	case policy.ReasonScopesKind:
		holder := "a stored secret"
		if set.Kind != "" {
			holder = fmt.Sprintf("a %s secret", set.Kind)
		}
		return usageErr{fmt.Errorf("--scopes narrows the token a %s secret mints, and no other kind's minter reads it, so %s holds none: remove the scopes tag with --scopes none, in a retag of its own, or write --kind %s, or the broker refuses it as %s", policy.KindGoogleServiceAccount, holder, policy.KindGoogleServiceAccount, policy.ReasonScopesKind)}
	case policy.ReasonAdminTier:
		return usageErr{fmt.Errorf("an admin secret releases an administrator's credential, which a person approves each release of, so it is human tier, not %q: write --tier %s, or the broker refuses it as %s", set.Tier, policy.TierHuman, policy.ReasonAdminTier)}
	case policy.ReasonAdminRequesters:
		return usageErr{fmt.Errorf("an admin secret names who may ask for an administrator's credential, never leaving it to owner and tier alone: write --requesters, or the broker refuses it as %s", policy.ReasonAdminRequesters)}
	case policy.ReasonMaxGrantRequired:
		demands := fmt.Sprintf("a %s secret", policy.KindGoogleServiceAccount)
		if set.Admin {
			demands = "an admin secret"
		}
		if set.MaxGrant == 0 {
			return usageErr{fmt.Errorf("%s releases a credential whose grant is reviewed while it is live, so it bounds its grants to an hour: name one with --max-grant 1h or less, or the broker refuses it as %s", demands, policy.ReasonMaxGrantRequired)}
		}
		// The cap too long is quoted as the tag holds it, which is how whoever named it wrote
		// it ("90m"), and not as the duration it parsed to ("1h30m0s"), which is the one thing
		// the set says less plainly than the map.
		return usageErr{fmt.Errorf("%s releases a credential whose grant is reviewed while it is live, so it bounds its grants to an hour, not %s: write --max-grant 1h or less, or the broker refuses it as %s", demands, tags[policy.TagMaxGrant], policy.ReasonMaxGrantRequired)}
	case policy.ReasonRequestersNotOwner:
		// Which of the requesters conditions refused the write is policy.RequesterRule's
		// answer, weighed there in the policy's own order, most fundamental first, so the flag
		// each arm names fixes the condition that refused it and not another that happens to be
		// true beside it: a secret owned by shared is answered for its owner however its list
		// and tier read, and a list naming a service at agent tier is answered for the tier and
		// not for the person it leaves out.
		switch policy.RequesterRule(set) {
		case policy.RequesterFaultOwnerNotPerson:
			return usageErr{fmt.Errorf("a %s secret mints a token acting as that account, so it is served to the one person who owns it, and %q is no person: own it with --owner me, or the broker refuses it as %s", set.Kind, set.Owner, policy.ReasonRequestersNotOwner)}
		case policy.RequesterFaultServiceBelowHumanTier:
			_, services := policy.SplitRequesters(set.Requesters)
			return usageErr{fmt.Errorf("a %s secret lists a registered service, here %q, only at %s tier, where a person approves each pod that asks: write --tier %s, or leave the service out of --requesters, or the broker refuses it as %s", set.Kind, services[0], policy.TierHuman, policy.TierHuman, policy.ReasonRequestersNotOwner)}
		case policy.RequesterFaultNotTheOwnerOnce:
			// One message for every way a list fails to name the owner exactly once — no list
			// at all, a list of services alone, a list naming another person — because one
			// write fixes them all: the owner, beside the registered services the list already
			// names, which a human-tier secret of this kind may keep. A list the secret holds
			// is quoted too, so the reader sees what the suggestion replaces.
			_, services := policy.SplitRequesters(set.Requesters)
			wanted := strings.Join(append([]string{set.Owner}, services...), " ")
			held := ""
			if set.Requesters != "" {
				held = fmt.Sprintf(", not %q", set.Requesters)
			}
			return usageErr{fmt.Errorf("a %s secret is served to the one person who owns it, beside the registered services a %s-tier one may list, so its requesters name %s once%s: write --requesters %q, or the broker refuses it as %s", set.Kind, policy.TierHuman, set.Owner, held, wanted, policy.ReasonRequestersNotOwner)}
		}
	}
	// Two things reach here: a reason no case above names, and a policy.RequesterFault no case of
	// the inner switch names, policy.RequesterFaultServiceOnDevAccount among them. That fault
	// turns on the deployment's binding of the secret to an account, which checkTagsFor never
	// holds and always passes as an empty BoundAccount, so no run of this CLI reaches it that
	// way, while a caller handing tagRefusal a set of its own can.
	return usageErr{fmt.Errorf("the broker refuses a secret carrying these tags as %s, and this agent-secrets has no message naming the flag that fixes it: write tags the broker accepts", reason)}
}

// optionalTags is the kind, github-owners, requesters, scopes, admin, max-grant and default-grant
// tags the flags set: each flag's value when it names one, and nothing for one left unset or set
// to none. A kind brings the rollback guard with it (policy.TagRollbackGuard), so a broker from
// before kinds refuses the secret rather than serve its stored key or refresh token.
func optionalTags(kind, githubOwners, requesters, scopes, admin, maxGrant, defaultGrant string) map[string]string {
	named := func(value string) bool { return value != "" && value != tagNone }
	tags := map[string]string{}
	if named(kind) {
		tags[policy.TagKind] = kind
		tags[policy.TagRollbackGuard] = policy.RollbackGuardValue
	}
	if named(githubOwners) {
		tags[policy.TagGitHubOwners] = githubOwners
	}
	if named(requesters) {
		tags[policy.TagRequesters] = requesters
	}
	if named(scopes) {
		tags[policy.TagScopes] = scopes
	}
	if named(admin) {
		tags[policy.TagAdmin] = admin
	}
	if named(maxGrant) {
		tags[policy.TagMaxGrant] = maxGrant
	}
	if named(defaultGrant) {
		tags[policy.TagDefaultGrant] = defaultGrant
	}
	return tags
}

// ownerTag is the owner tag --owner names: the signed-in person's email for me, shared for shared.
func ownerTag(owner, email string) (string, error) {
	if owner == "me" {
		if !policy.ValidPersonOwner(email) {
			return "", usageErr{fmt.Errorf("your sign-in's email %q is not an email the broker accepts as an owner", email)}
		}
		return email, nil
	}
	return owner, nil
}

// checkDefaultGrant is --default-grant's rule: "session", or none where orNone allows it.
func checkDefaultGrant(value string, orNone bool) error {
	if value == "" || value == policy.DefaultGrantSession || (orNone && value == tagNone) {
		return nil
	}
	return usageErr{fmt.Errorf("--default-grant: %q is not session", value)}
}
