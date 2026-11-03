package store

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/routing"
)

// cachedRegistry is a registry whose cache holds items, each written through the cache as a watcher
// update is, so its subject index holds them too.
func cachedRegistry(items map[string]Interest) *Registry {
	r := &Registry{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, item := range items {
		r.cacheInterestLocked(key, item, 1)
	}
	return r
}

// indexMatches is the sorted keys of machineID that index matches subject with, each once.
func indexMatches(x *subjectIndex, machineID, subject string) []string {
	var keys []string
	x.match(machineID, subject, func(key string) {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	})
	slices.Sort(keys)
	return keys
}

// routingMatches is the sorted keys of machineID with a pattern routing.Match matches subject with.
func routingMatches(items map[string]Interest, machineID, subject string) []string {
	var keys []string
	for key, item := range items {
		if item.MachineID != machineID {
			continue
		}
		if slices.ContainsFunc(item.Topics, func(pattern string) bool { return routing.Match(pattern, subject) }) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func TestSubjectIndexMatchesAsRoutingMatchDoes(t *testing.T) {
	for _, tc := range []struct {
		pattern, subject string
		want             bool
	}{
		{"notifications.github.sjawhar.legion.pr", "notifications.github.sjawhar.legion.pr", true},
		{"notifications.github.sjawhar.legion.pr", "notifications.github.sjawhar.legion.pr.1", false},
		{"notifications.github.sjawhar.legion.pr.1", "notifications.github.sjawhar.legion.pr", false},
		{"notifications.github.*.*.pr", "notifications.github.sjawhar.legion.pr", true},
		{"notifications.github.*.*.pr", "notifications.github.sjawhar.legion.issue", false},
		{"notifications.github.*", "notifications.github", false},
		{"notifications.github.>", "notifications.github.a", true},
		{"notifications.github.>", "notifications.github.a.b.c", true},
		{"notifications.github.>", "notifications.github", false},
		{"notifications.>", "notifications.github", true},
		{">", "notifications", true},
		{">", "", true},
		{"*", "notifications", true},
		{"*", "notifications.github", false},
		{"*.>", "notifications.github", true},
		{"*.>", "notifications", false},
		// ">" ends a pattern wherever it stands: what follows it is never compared.
		{"notifications.>.pr", "notifications.github", true},
		{"notifications.>.pr", "notifications.github.legion.issue", true},
		// Tokens split on every ".", an empty one included, as routing.Match splits them.
		{"notifications..pr", "notifications..pr", true},
		{"notifications.*.pr", "notifications..pr", true},
		{"notifications.>", "notifications.", true},
		{"", "", true},
		{"", "notifications", false},
		{"notifications", "", false},
	} {
		if got := routing.Match(tc.pattern, tc.subject); got != tc.want {
			t.Fatalf("routing.Match(%q, %q) = %v, the table says %v", tc.pattern, tc.subject, got, tc.want)
		}
		x := newSubjectIndex()
		x.set("ses", Interest{MachineID: "m", Topics: []string{tc.pattern}})
		if got := len(indexMatches(x, "m", tc.subject)) == 1; got != tc.want {
			t.Errorf("index of %q matches %q: %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
		if got := indexMatches(x, "other", tc.subject); len(got) != 0 {
			t.Errorf("index of %q on machine m matched %q for another machine: %v", tc.pattern, tc.subject, got)
		}
	}
}

// randomTokens joins one to five tokens drawn from alphabet.
func randomTokens(rng *rand.Rand, alphabet []string) string {
	tokens := make([]string, 1+rng.IntN(5))
	for i := range tokens {
		tokens[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return strings.Join(tokens, ".")
}

// Over generated interests, patterns and subjects, the index matches exactly the keys a scan with
// routing.Match does, as interests are added, replaced and removed; and once every one is removed it
// holds nothing.
func TestSubjectIndexAgreesWithRoutingMatchOnGeneratedInterests(t *testing.T) {
	rng := rand.New(rand.NewPCG(594, 10))
	patternTokens := []string{"a", "b", "c", "", "*", ">"}
	subjectTokens := []string{"a", "b", "c", ""}
	machines := []string{"m1", "m2", ""}
	for round := range 200 {
		x := newSubjectIndex()
		items := map[string]Interest{}
		randomInterest := func() Interest {
			topics := make([]string, 1+rng.IntN(3))
			for i := range topics {
				topics[i] = randomTokens(rng, patternTokens)
			}
			return Interest{MachineID: machines[rng.IntN(len(machines))], Topics: topics}
		}
		compare := func(step string) {
			t.Helper()
			for range 40 {
				subject := randomTokens(rng, subjectTokens)
				for _, machineID := range machines {
					if got, want := indexMatches(x, machineID, subject), routingMatches(items, machineID, subject); !slices.Equal(got, want) {
						t.Fatalf("round %d, %s: machine %q subject %q: index matched %v, routing.Match %v; interests %v", round, step, machineID, subject, got, want, items)
					}
				}
			}
		}
		keys := []string{"s0", "s1", "s2", "s3", "s4", "s5", "s6", "s7"}
		for _, key := range keys {
			items[key] = randomInterest()
			x.set(key, items[key])
		}
		compare("added")
		for _, key := range keys[:4] {
			items[key] = randomInterest()
			x.set(key, items[key])
		}
		compare("replaced")
		for _, key := range keys[4:] {
			delete(items, key)
			x.remove(key)
		}
		compare("removed")
		for _, key := range keys[:4] {
			delete(items, key)
			x.remove(key)
		}
		if len(x.roots) != 0 || len(x.entries) != 0 {
			t.Fatalf("round %d: with every interest removed the index still holds %d trees and %d entries", round, len(x.roots), len(x.entries))
		}
	}
}

// The registry's index follows its interest bucket: a subscription another listener writes is
// matched once the watcher delivers it, a topic removed or an interest deleted stops matching, and a
// machine's index holds only that machine's interests.
func TestSubjectIndexFollowsTheInterestBucket(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	r, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("open the registry: %v", err)
	}
	defer r.StopWatch()
	other, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("open the other listener's registry: %v", err)
	}
	defer other.StopWatch()

	const topic = "notifications.dispatch.issue.IDX-1.comment"
	if _, err := other.Upsert(Interest{SessionID: "ses_idx", MachineID: "m1"}, []string{"notifications.dispatch.issue.IDX-1.>", "notifications.agent.ses_idx"}); err != nil {
		t.Fatalf("subscribe through the other registry: %v", err)
	}
	got := pollMatch(t, r, "m1", topic, 1, 5*time.Second)
	if got[0].SessionID != "ses_idx" {
		t.Fatalf("matched %v, want ses_idx", got)
	}
	twice := cachedRegistry(map[string]Interest{"ses_twice": {SessionID: "ses_twice", MachineID: "m1", Topics: []string{"notifications.dispatch.issue.IDX-1.>", "notifications.dispatch.issue.*.comment"}}})
	if got := twice.Match("m1", topic); len(got) != 1 || got[0].SessionID != "ses_twice" {
		t.Fatalf("an interest two of whose topics match was matched as %v, want once", got)
	}
	if got := r.Match("m2", topic); len(got) != 0 {
		t.Fatalf("machine m2 matched %v, another machine's interest", got)
	}

	if err := other.Remove(t.Context(), "ses_idx", []string{"notifications.dispatch.issue.IDX-1.>"}); err != nil {
		t.Fatalf("unsubscribe the topic: %v", err)
	}
	pollMatch(t, r, "m1", topic, 0, 5*time.Second)
	if got := r.Match("m1", "notifications.agent.ses_idx"); len(got) != 1 {
		t.Fatalf("the session's remaining topic matched %v, want the session", got)
	}

	if _, err := r.Upsert(Interest{SessionID: "ses_idx", MachineID: "m1"}, []string{"notifications.dispatch.issue.*.comment"}); err != nil {
		t.Fatalf("subscribe a wildcard: %v", err)
	}
	if got := r.Match("m1", topic); len(got) != 1 {
		t.Fatalf("a registry's own write matched %v at once, want the session", got)
	}
	if err := other.deleteInterest(t.Context(), "ses_idx"); err != nil {
		t.Fatalf("delete the interest: %v", err)
	}
	pollMatch(t, r, "m1", topic, 0, 5*time.Second)
	pollMatch(t, r, "m1", "notifications.agent.ses_idx", 0, 5*time.Second)
}
