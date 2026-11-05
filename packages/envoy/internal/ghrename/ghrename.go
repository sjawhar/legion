// Package ghrename holds the repository renames a deployment configures
// (ENVOY_GITHUB_REPOSITORY_RENAMES) and answers, for a subject the listener is about to publish on,
// the same subject under the former name of the repository that subject names.
//
// A rename on GitHub does not move a subscription. GitHub's events name the repository it holds
// now, so the listener publishes on `notifications.github.cybermonklabs.legion.…` while every
// session that subscribed before the move still waits on `notifications.github.sjawhar.legion.…`
// and hears nothing. Publishing each envelope a second time under the former name is what keeps
// those subscriptions delivering, for as long as the pair stays configured.
//
// The mapping is one-way: a subject naming the current repository answers the former name, never
// the reverse. The former name is the compatibility address, and nothing publishes under it first.
package ghrename

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// Rename is one configured pair: the repository as it was named, and the one GitHub holds it under
// now. Each side is an `<owner>/<name>`, spelled as the value spelled it, less the whitespace
// around it.
type Rename struct{ Former, Current string }

// Renames holds the configured pairs. The zero value holds none and answers nothing for every
// subject, so a deployment that configures no rename publishes exactly what it always published.
type Renames struct {
	// pairs are the configured pairs, in configured order.
	pairs []Rename
	// former is each pair's former subject prefix and the pair itself, keyed by the lowercased
	// subject prefix of its current repository: one lookup per publish answers both what to
	// rewrite the subject to and which pair to name in a log line, and nil in the zero value so a
	// deployment that configures no rename pays a nil map read.
	former map[string]renamed
}

// renamed is what one configured pair answers for a subject of its current repository: the subject
// prefix to put in that subject's place, and the pair as the value spelled it, for a log line.
type renamed struct {
	formerPrefix string
	pair         Rename
}

// Parse reads the value of ENVOY_GITHUB_REPOSITORY_RENAMES, a comma-separated list of
// `<former owner>/<name>=<current owner>/<name>` — or a refusal naming the entry it cannot hold and
// what in it was refused, since the listener prints that line and nothing else when it declines to
// start, and an operator has to fix the value from it alone.
//
// An empty or all-whitespace value configures no rename and answers the zero Renames. Entries and
// each side of an entry are trimmed, and an empty entry between commas is skipped, so a value
// written over several shell lines reads as one written on one. An entry's index is its place in
// the comma-separated value, counting skipped empty entries, which is the entry an operator counts.
//
// Two names are one repository when they are one subject prefix, compared without case: GitHub
// compares names without case, and a name holding a dot is written with an underscore in a subject
// (`acme/site.io` publishes under `acme.site_io`), so two names a subject cannot tell apart are one
// repository here however they are spelled.
func Parse(value string) (Renames, error) {
	if strings.TrimSpace(value) == "" {
		return Renames{}, nil
	}

	// entry is one accepted entry, kept to refuse a later one that names a repository it names.
	type entry struct {
		index        int
		text         string
		pair         Rename
		formerPrefix string // the former repository's subject prefix, as configured
		former       string // formerPrefix, lowercased: the key a comparison reads
		current      string // the current repository's subject prefix, lowercased
	}

	var accepted []entry
	for index, text := range strings.Split(value, ",") {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		formerName, currentName, split := strings.Cut(text, "=")
		if !split || strings.Contains(currentName, "=") {
			return Renames{}, fmt.Errorf("rename %d (%q) is no `<former owner>/<name>=<current owner>/<name>`: one = separates the former repository from the current one", index, text)
		}
		formerName, currentName = strings.TrimSpace(formerName), strings.TrimSpace(currentName)
		formerPrefix, ok := repositoryPrefix(formerName)
		if !ok {
			return Renames{}, fmt.Errorf("rename %d names %q as the former repository, which is no `<owner>/<name>`: an owner and a name, both non-empty, parted by one /", index, formerName)
		}
		currentPrefix, ok := repositoryPrefix(currentName)
		if !ok {
			return Renames{}, fmt.Errorf("rename %d names %q as the current repository, which is no `<owner>/<name>`: an owner and a name, both non-empty, parted by one /", index, currentName)
		}
		current := entry{
			index:        index,
			text:         text,
			pair:         Rename{Former: formerName, Current: currentName},
			formerPrefix: formerPrefix,
			former:       strings.ToLower(formerPrefix),
			current:      strings.ToLower(currentPrefix),
		}
		if current.former == current.current {
			return Renames{}, fmt.Errorf("rename %d (%q) names one repository, %s, as its former and its current name: the two sides are two repositories, compared without case", index, text, currentName)
		}
		for _, earlier := range accepted {
			switch {
			case current.former == earlier.former:
				return Renames{}, fmt.Errorf("rename %d (%q) and rename %d (%q) both name %s as the former repository: a repository is renamed from one name", index, text, earlier.index, earlier.text, formerName)
			case current.current == earlier.current:
				return Renames{}, fmt.Errorf("rename %d (%q) and rename %d (%q) both name %s as the current repository: a repository is renamed to one name", index, text, earlier.index, earlier.text, currentName)
			case current.former == earlier.current:
				return Renames{}, fmt.Errorf("rename %d (%q) chains onto rename %d (%q) through %s, the former repository of the one and the current repository of the other: a rename pairs two names, never a chain of three", index, text, earlier.index, earlier.text, formerName)
			case current.current == earlier.former:
				return Renames{}, fmt.Errorf("rename %d (%q) chains onto rename %d (%q) through %s, the current repository of the one and the former repository of the other: a rename pairs two names, never a chain of three", index, text, earlier.index, earlier.text, currentName)
			}
		}
		accepted = append(accepted, current)
	}
	if len(accepted) == 0 {
		return Renames{}, nil
	}

	renames := Renames{
		pairs:  make([]Rename, 0, len(accepted)),
		former: make(map[string]renamed, len(accepted)),
	}
	for _, kept := range accepted {
		renames.pairs = append(renames.pairs, kept.pair)
		renames.former[kept.current] = renamed{formerPrefix: kept.formerPrefix, pair: kept.pair}
	}
	return renames, nil
}

// rewrite is subject under the former name of the pair whose current name it names, that pair, and
// whether a pair names the repository at all. The subject is rewritten only when its first four
// dot-separated tokens are `notifications`, `github` and that pair's current owner and name, and
// at least one token follows: the four alone name a repository rather than an event, and nothing
// publishes on them. Everything after the fourth token is carried over byte for byte, so the copy
// is the same event, addressed to the repository as it was named.
//
// The rewrite is answered whatever its length: its one caller, the publisher, declines one past
// contracts.SubjectMaxBytes and logs that one line. One walk of the subject and one map read
// answer all three, which is what every GitHub publish pays.
func (r Renames) rewrite(subject string) (former string, pair Rename, named bool) {
	// Every GitHub publish asks this, and a deployment that configures no rename asks it of the
	// zero value: the nil map answers before anything is measured or lowercased.
	if r.former == nil {
		return "", Rename{}, false
	}
	end := repositoryPrefixEnd(subject)
	if end < 0 {
		return "", Rename{}, false
	}
	// A published subject is already lowercase wherever GitHub's own names are, and ToLower
	// answers an all-lowercase ASCII string unchanged, so a subject no pair names allocates
	// nothing on its way to the miss.
	renamed, found := r.former[strings.ToLower(subject[:end])]
	if !found {
		return "", Rename{}, false
	}
	return renamed.formerPrefix + subject[end:], renamed.pair, true
}

// IsZero reports whether r holds no pair, the zero value a deployment that configures none carries.
func (r Renames) IsZero() bool { return len(r.pairs) == 0 }

// Pairs are the configured pairs, in configured order, as a copy no caller can use to change what
// the renames answer.
func (r Renames) Pairs() []Rename { return slices.Clone(r.pairs) }

// String spells, for each configured pair in configured order, the pair as
// ENVOY_GITHUB_REPOSITORY_RENAMES spells it and the subject prefix that pair's copies are
// published under: `<former>=<current> copies to <prefix>`, joined by `, `. The value's grammar
// is written here, in the package that parses it, so a caller that prints the renames cannot
// spell them another way. The zero Renames is the empty string.
//
// The prefix is spelled out because the former name is written into the subject exactly as the
// value spells it, while the current name is matched without case. A NATS subject is compared
// with case, and a subscriber holds the subject GitHub's own spelling of the repository
// produced, so a former name written in a case GitHub never used addresses every copy to a
// subject nothing subscribes to. The listener logs this line at start, so that operator reads
// the subject in the log's first lines rather than meeting the silence later.
func (r Renames) String() string {
	spelled := make([]string, 0, len(r.pairs))
	for _, pair := range r.pairs {
		// Parse accepted the pair, so both of its sides are an `<owner>/<name>` and this holds.
		prefix, _ := repositoryPrefix(pair.Former)
		spelled = append(spelled, pair.Former+"="+pair.Current+" copies to "+prefix)
	}
	return strings.Join(spelled, ", ")
}

// repositoryPrefix is the subject prefix every envelope of the repository name names is published
// on, and whether name is an `<owner>/<name>` at all: an owner and a name, both non-empty, parted
// by one /. The owner and the name are each sanitized exactly as a published subject writes them,
// because a name holding a dot (`acme/site.io`) is written `acme.site_io` and a comparison against
// the configured spelling would never match a subject.
func repositoryPrefix(name string) (string, bool) {
	owner, repository, found := strings.Cut(name, "/")
	if !found || owner == "" || repository == "" || strings.Contains(repository, "/") {
		return "", false
	}
	return contracts.GithubRepositoryPrefix(owner, repository), true
}

// repositoryPrefixEnd is the index in subject of the dot that closes
// `notifications.github.<owner>.<name>`, or -1 when subject is no GitHub repository subject with a
// token after the name. A sanitized owner and name hold no dot, so the fourth dot ends the prefix
// whatever the event carries after it.
func repositoryPrefixEnd(subject string) int {
	if !strings.HasPrefix(subject, contracts.GithubTopicPrefix) {
		return -1
	}
	owner := strings.IndexByte(subject[len(contracts.GithubTopicPrefix):], '.')
	if owner < 0 {
		return -1
	}
	rest := len(contracts.GithubTopicPrefix) + owner + 1
	name := strings.IndexByte(subject[rest:], '.')
	if name < 0 || rest+name == len(subject)-1 {
		return -1
	}
	return rest + name
}
