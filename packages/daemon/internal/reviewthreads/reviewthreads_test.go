package reviewthreads

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
)

// The rule's vectors. The subject of a finding never closes it. A thread closes on its opener's
// Accepted:, or, when a Bot that is none of Legion's role Apps opened it (a CI bot, or a person
// whose gh is routed to an App: GitHub cannot tell them apart), on Legion's review App's, the
// independent party; the acceptance says which. The pull request author's reply (Fixed in,
// Declined, its own Accepted:) closes nothing. Accepted: counts only as the first line of a
// submitted comment, after space, tab, CR or LF alone, whatever the login's case. A thread either
// Legion App opened closes only on its opener's Accepted:, and a person's thread only on its
// opener's. An account is its type and its login together: a User who registered the review App's
// bare slug is not the review App, nor the Bot opener of that name. A caller that cannot know
// Legion's Apps leaves every bot's thread to its opener.
func TestResolutionClosesAThreadOnlyOnItsOpenersOrTheLegionReviewersAcceptance(t *testing.T) {
	apps, err := AppsFrom(map[appauth.AppRole]string{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"})
	if err != nil {
		t.Fatal(err)
	}
	const notEither = "not its opener's or the Legion reviewer's acceptance"
	bot := func(opener, newestType, newest, body string) Thread {
		return Thread{OpenerTypename: "Bot", OpenerLogin: opener, NewestTypename: newestType, NewestLogin: newest, NewestBody: body}
	}
	for _, tc := range []struct {
		name   string
		thread Thread
		apps   *Apps
		want   Acceptance
		reason string
	}{
		{"the reviewer accepts a bot's finding", bot("claude", "Bot", "legion-reviewer", "Accepted: fixed in 1a2b3c4 — moved the guard"), apps, ReviewersAcceptanceOfABot, ""},
		{"in any case, after space, tab, CR and LF", bot("claude", "Bot", "Legion-Reviewer", " \t\r\nAccepted: not a defect — the loop is bounded"), apps, ReviewersAcceptanceOfABot, ""},
		{"the author declines", bot("claude", "Bot", "legion-implementer", "Declined: the loop is bounded"), apps, "", notEither},
		{"the author says it fixed it", bot("claude", "Bot", "legion-implementer", "Fixed in 1a2b3c4: moved the guard"), apps, "", notEither},
		{"the author accepts", bot("claude", "Bot", "legion-implementer", "Accepted: my own fix"), apps, "", notEither},
		{"the reviewer says it still stands", bot("claude", "Bot", "legion-reviewer", "Still open: the loop is not bounded"), apps, "", notEither},
		{"a no-break space before Accepted:", bot("claude", "Bot", "legion-reviewer", "\u00a0Accepted: fixed"), apps, "", notEither},
		{"Accepted: on a second line", bot("claude", "Bot", "legion-reviewer", "Thanks.\nAccepted: fixed"), apps, "", notEither},
		{"the reviewer's draft", Thread{OpenerTypename: "Bot", OpenerLogin: "claude", NewestTypename: "Bot", NewestLogin: "legion-reviewer", NewestBody: "Accepted: drafted", NewestPending: true}, apps, "", "an unsubmitted draft in a pending review"},
		{"a routed person's finding the reviewer accepts", bot("sjawhar-agent", "Bot", "legion-reviewer", "Accepted: fixed in 1a2b3c4 — moved the guard"), apps, ReviewersAcceptanceOfABot, ""},
		{"a routed person accepting its own", bot("sjawhar-agent", "Bot", "sjawhar-agent", "Accepted: fixed"), apps, OpenersAcceptance, ""},
		{"the reviewer's thread the author answered", bot("legion-reviewer", "Bot", "legion-implementer", "Fixed in 1a2b3c4: moved the guard"), apps, "", "not an acceptance"},
		{"the implementer App's thread the reviewer accepts", bot("legion-implementer", "Bot", "legion-reviewer", "Accepted: fine"), apps, "", "not an acceptance"},
		{"a person's thread the reviewer accepts", Thread{OpenerTypename: "User", OpenerLogin: "octocat", NewestTypename: "Bot", NewestLogin: "legion-reviewer", NewestBody: "Accepted: fixed"}, apps, "", "not an acceptance"},
		{"a User named as the review App accepts a bot's", bot("claude", "User", "legion-reviewer", "Accepted: fixed"), apps, "", notEither},
		{"a User named as the review App accepts the review App's", bot("legion-reviewer", "User", "legion-reviewer", "Accepted: fixed"), apps, "", "not an acceptance"},
		{"a person accepts their own after space, tab, CR and LF", Thread{OpenerLogin: "reviewer", NewestLogin: "reviewer", NewestBody: " \t\r\nAccepted: fixed"}, apps, OpenersAcceptance, ""},
		{"a person's no-break space before Accepted:", Thread{OpenerLogin: "reviewer", NewestLogin: "reviewer", NewestBody: "\u00a0Accepted: fixed"}, apps, "", "not an acceptance"},
		{"a bot's thread the reviewer accepts, Legion's Apps unknown", bot("claude", "Bot", "legion-reviewer", "Accepted: fixed"), nil, "",
			"not its opener's acceptance, and this session cannot identify Legion's review App, so a bot's thread closes only on its opener's Accepted:"},
		{"a person's thread, Legion's Apps unknown", Thread{OpenerTypename: "User", OpenerLogin: "octocat", NewestTypename: "Bot", NewestLogin: "legion-implementer", NewestBody: "Fixed in 1a2b3c4: moved the guard"}, nil, "", "not an acceptance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, reason := Resolution(tc.thread, tc.apps); got != tc.want || reason != tc.reason {
				t.Fatalf("Resolution = %q, %q; want %q, %q", got, reason, tc.want, tc.reason)
			}
		})
	}
}
