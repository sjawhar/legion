// packages/envoy/cmd/agent-secrets/secret_retag.go
//
// The retag form: a secret's tag change, written against the tags it was checked on.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// heldTags is a secret's tags as one read of the store held them: AWS tags or Secret Manager
// annotations, and, on GCP, that read's etag ("" on AWS), which a tag write sends so it lands only
// on the tags it was checked against.
type heldTags struct {
	tags map[string]string
	etag string
}

// errTagsChanged is a tag write another write raced: it changed the secret after the read the
// write was made against. GCP refuses the write (an etag that no longer matches), so nothing is
// written; AWS, whose tag calls take no condition, finds the race by reading the tags back and
// restores the tags the retag read (awsBackend.raceCheck), wrapping errTagsRestored too.
var errTagsChanged = errors.New("another write changed the secret after this retag read it")

// errTagsRestored is the part of an AWS errTagsChanged that says the write landed and was then
// put back to the tags the retag read, which the broker must be asked to reread.
var errTagsRestored = errors.New("the tags are restored to what this retag read; run the retag again once the other writer is done")

// cmdSecretRetag changes a secret's owner, tier, kind, GitHub owners or max grant, in one cloud
// write. A retag that names none (--kind none, --github-owners none, --max-grant none) removes
// those tags and does nothing else; --kind none also removes the rollback guard written with the
// kind. Any other retag sets tags. A retag that does both is refused before anything is read or
// written: AWS removes tags only with UntagResource, so it would be two calls, and a refused second
// call would leave it half applied.
//
// Either kind of retag reads the tags the secret holds, builds from them the tags it will hold once
// the write lands (after), and checks the github-owners rule against those once
// (checkGitHubOwnersFor), so neither leaves a github-app secret without GitHub owners, nor names
// owners for a kind that does not read them. The write is made against that read (heldTags), and a
// write another writer raced answers errTagsChanged, which the retag prints, exiting 1; on AWS,
// whose write then restored the tags it read (errTagsRestored), it also asks the broker to reread.
//
// A retag that sets tags always sends owner and tier, re-sending the one not named as the secret
// holds it: on AWS, the deployment repository's IAM policy that lets a person tag their own secret
// conditions on both request tags, so a request carrying one fails. GCP has no equivalent
// condition (annotations are not IAM-conditionable the way AWS tags are): anyone with
// secretmanager.secrets.update on the namespace can retag any secret under it, shared ones
// included; sending both tags there is for API symmetry with AWS, not an enforced shared-secret
// protection.
func cmdSecretRetag(args []string, stdout, stderr io.Writer) int {
	const form = "secret retag"
	flagArgs, positional := splitArgs(args, secretFlagValues)
	flags, profile := secretFlags(form, stderr)
	newOwner := flags.String("owner", "", "the new owner: me (your email, from your AWS or Google sign-in) or shared")
	newTier := flags.String("tier", "", "the new tier: agent or human")
	newKind := flags.String("kind", "", "github-app, google-login or none")
	newGitHubOwners := flags.String("github-owners", "", `the GitHub accounts a github-app secret mints for, separated by spaces ("<login> <login>"), or none`)
	newMaxGrant := flags.String("max-grant", "", "the longest grant duration this secret allows, or none")
	if err := flags.Parse(flagArgs); err != nil {
		return exitUsage(err)
	}
	name, slug, err := secretName(positional)
	if err == nil && *newOwner == "" && *newTier == "" && *newKind == "" && *newGitHubOwners == "" && *newMaxGrant == "" {
		err = usageErr{errors.New("--owner, --tier, --kind, --github-owners, --max-grant or a combination is required")}
	}
	if err == nil {
		err = checkOwnerTier(*newOwner, *newTier)
	}
	if err == nil {
		err = checkKind(*newKind)
	}
	if err == nil {
		*newGitHubOwners, err = checkGitHubOwners(*newGitHubOwners, true)
	}
	if err == nil {
		err = checkMaxGrant(*newMaxGrant, true)
	}
	var remove []string
	if *newKind == tagNone {
		// The rollback guard goes with the kind: a stored secret is safe on every broker.
		remove = append(remove, policy.TagKind, policy.TagRollbackGuard)
	}
	if *newGitHubOwners == tagNone {
		remove = append(remove, policy.TagGitHubOwners)
	}
	if *newMaxGrant == tagNone {
		remove = append(remove, policy.TagMaxGrant)
	}
	set := optionalTags(*newKind, *newGitHubOwners, *newMaxGrant)
	if err == nil && len(remove) > 0 && (*newOwner != "" || *newTier != "" || len(set) > 0) {
		err = usageErr{errors.New("none removes a tag in a retag of its own: run the removal and the other changes as two retags")}
	}
	if err != nil {
		return secretFail(stderr, form, err)
	}
	s, email, err := connectWriter(context.Background(), *profile)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	defer s.Close()
	held, err := s.backend.currentTags(s.ctx, slug)
	if err != nil {
		return secretFail(stderr, form, err)
	}
	// after is the secret's tags once this retag lands: the tags held, less those it removes, with
	// those it sets.
	after := maps.Clone(held.tags)
	if after == nil {
		after = map[string]string{}
	}
	for _, key := range remove {
		delete(after, key)
	}
	maps.Copy(after, set)
	if *newOwner != "" {
		if after[policy.TagOwner], err = ownerTag(*newOwner, email); err != nil {
			return secretFail(stderr, form, err)
		}
	}
	if *newTier != "" {
		after[policy.TagTier] = *newTier
	}
	named := *newGitHubOwners != "" && *newGitHubOwners != tagNone
	if err := checkGitHubOwnersFor(after[policy.TagKind], after[policy.TagGitHubOwners], named); err != nil {
		return secretFail(stderr, form, err)
	}
	if len(remove) > 0 {
		if err := s.backend.removeTags(s.ctx, slug, held, remove); err != nil {
			fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
			rereadIfRestored(s, stdout, stderr, form, name, err)
			return 1
		}
		fmt.Fprintf(stdout, "retagged %s (removed %s)\n", name, strings.Join(remove, ", "))
		return rereadAfter(s, stdout, stderr, form, name, true, nil)
	}
	owner, tier := after[policy.TagOwner], after[policy.TagTier]
	if owner == "" || tier == "" {
		return secretFail(stderr, form, usageErr{fmt.Errorf("%s has no owner tag or no tier tag, and a retag sends both: name the missing one with --owner or --tier", name)})
	}
	set[policy.TagOwner], set[policy.TagTier] = owner, tier
	if err := s.backend.setTags(s.ctx, slug, held, set); err != nil {
		fmt.Fprintf(stderr, "agent-secrets %s: %v\n", form, err)
		// AWS: IAM conditioned on the request tags leaves a shared secret's owner and tier to
		// administrators specifically. GCP has no per-secret condition on secretmanager.secrets.update,
		// so a deployment instead grants that permission at all only to administrators for the
		// secrets it protects this way.
		if s.backend.isAccessDenied(err) && (held.tags[policy.TagOwner] == policy.OwnerShared || owner == policy.OwnerShared) {
			fmt.Fprintf(stderr, "agent-secrets %s: %s\n", form, sharedRetagRefused)
		}
		rereadIfRestored(s, stdout, stderr, form, name, err)
		return 1
	}
	fmt.Fprintf(stdout, "retagged %s (owner=%s, tier=%s)\n", name, owner, tier)
	return rereadAfter(s, stdout, stderr, form, name, true, nil)
}

// rereadIfRestored asks the broker to reread name after a tag write the store wrote and then put
// back, because a racing writer left it a pair the broker refuses (errTagsRestored, AWS): the
// secret's tags changed twice, so the broker is asked to serve them as they now stand. A write the
// store refused wrote nothing (GCP's errTagsChanged alone), and needs no reread.
func rereadIfRestored(s *secretSession, stdout, stderr io.Writer, form, name string, err error) {
	if errors.Is(err, errTagsRestored) {
		rereadAfter(s, stdout, stderr, form, name, true, nil)
	}
}
