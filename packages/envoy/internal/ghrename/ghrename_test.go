package ghrename

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []Rename
	}{
		{name: "unset", value: "", want: nil},
		{name: "all whitespace", value: "  \t ", want: nil},
		{
			name:  "one pair",
			value: "sjawhar/legion=cybermonklabs/legion",
			want:  []Rename{{Former: "sjawhar/legion", Current: "cybermonklabs/legion"}},
		},
		{
			name:  "two pairs",
			value: "sjawhar/legion=cybermonklabs/legion,acme/old=acme/new",
			want: []Rename{
				{Former: "sjawhar/legion", Current: "cybermonklabs/legion"},
				{Former: "acme/old", Current: "acme/new"},
			},
		},
		{
			name:  "whitespace around entries and sides",
			value: "  sjawhar/legion = cybermonklabs/legion , \tacme/old\t=\tacme/new ",
			want: []Rename{
				{Former: "sjawhar/legion", Current: "cybermonklabs/legion"},
				{Former: "acme/old", Current: "acme/new"},
			},
		},
		{
			name:  "empty entry between commas",
			value: "sjawhar/legion=cybermonklabs/legion,,acme/old=acme/new,",
			want: []Rename{
				{Former: "sjawhar/legion", Current: "cybermonklabs/legion"},
				{Former: "acme/old", Current: "acme/new"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			renames, err := Parse(tc.value)
			if err != nil {
				t.Fatalf("Parse(%q) refused the value: %v, want the pairs it holds", tc.value, err)
			}
			if renames.IsZero() != (len(tc.want) == 0) {
				t.Errorf("Parse(%q).IsZero() = %v, want %v", tc.value, renames.IsZero(), len(tc.want) == 0)
			}
			pairs := renames.Pairs()
			if len(pairs) != len(tc.want) {
				t.Fatalf("Parse(%q).Pairs() = %v, want %v", tc.value, pairs, tc.want)
			}
			for i, want := range tc.want {
				if pairs[i] != want {
					t.Errorf("Parse(%q).Pairs()[%d] = %+v, want %+v", tc.value, i, pairs[i], want)
				}
			}
		})
	}
}

// TestParsePairsIsACopy covers the promise Pairs makes: a caller who writes into what it answers
// changes nothing the renames hold, so a pair cannot be edited out from under a publish.
func TestParsePairsIsACopy(t *testing.T) {
	renames, err := Parse("sjawhar/legion=cybermonklabs/legion")
	if err != nil {
		t.Fatalf("Parse refused one pair: %v, want the pair it holds", err)
	}
	renames.Pairs()[0] = Rename{Former: "someone/else", Current: "someone/other"}
	if got := renames.Pairs()[0]; got.Former != "sjawhar/legion" {
		t.Errorf("after writing into Pairs(), Pairs()[0] = %+v, want the configured sjawhar/legion pair", got)
	}
}

func TestParseRefusals(t *testing.T) {
	cases := []struct {
		name  string
		value string
		// wantNames is what the refusal must name so an operator can fix the value from the
		// listener's one refusal line.
		wantNames []string
	}{
		{
			name:      "an entry with no =",
			value:     "sjawhar/legion",
			wantNames: []string{"rename 0", `"sjawhar/legion"`, "former", "current"},
		},
		{
			name:      "an entry with two =",
			value:     "sjawhar/legion=cybermonklabs/legion=acme/new",
			wantNames: []string{"rename 0", `"sjawhar/legion=cybermonklabs/legion=acme/new"`, "former", "current"},
		},
		{
			name:      "a former side that is no owner/name",
			value:     "acme/old=acme/new,legion=cybermonklabs/legion",
			wantNames: []string{"rename 1", `"legion"`, "former"},
		},
		{
			name:      "a former side with an empty owner",
			value:     "/legion=cybermonklabs/legion",
			wantNames: []string{"rename 0", `"/legion"`, "former"},
		},
		{
			name:      "a current side with a second /",
			value:     "sjawhar/legion=cybermonklabs/legion/main",
			wantNames: []string{"rename 0", `"cybermonklabs/legion/main"`, "current"},
		},
		{
			name:      "a pair whose two sides are one repository",
			value:     "sjawhar/Legion=SJAWHAR/legion",
			wantNames: []string{"rename 0", "SJAWHAR/legion", "former", "current"},
		},
		{
			name:      "a repository named as the former of two pairs",
			value:     "sjawhar/legion=cybermonklabs/legion,SJAWHAR/legion=acme/new",
			wantNames: []string{"rename 0", "rename 1", "SJAWHAR/legion", "former"},
		},
		{
			name:      "a repository named as the current of two pairs",
			value:     "sjawhar/legion=cybermonklabs/legion,acme/old=CYBERMONKLABS/legion",
			wantNames: []string{"rename 0", "rename 1", "CYBERMONKLABS/legion", "current"},
		},
		{
			name:      "a chain of one pair's current onto another's former",
			value:     "sjawhar/legion=cybermonklabs/legion,cybermonklabs/legion=acme/new",
			wantNames: []string{"rename 0", "rename 1", "cybermonklabs/legion", "chains"},
		},
		{
			name:      "a chain of one pair's former onto another's current",
			value:     "sjawhar/legion=cybermonklabs/legion,acme/old=sjawhar/legion",
			wantNames: []string{"rename 0", "rename 1", "sjawhar/legion", "chains"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			renames, err := Parse(tc.value)
			if err == nil {
				t.Fatalf("Parse(%q) held %v, want a refusal", tc.value, renames.Pairs())
			}
			if !renames.IsZero() {
				t.Errorf("Parse(%q) refused the value and answered %v, want the zero Renames", tc.value, renames.Pairs())
			}
			for _, want := range tc.wantNames {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Parse(%q) refused with %q, want a refusal naming %q", tc.value, err, want)
				}
			}
		})
	}
}

// TestRewrite covers which subjects name a configured pair's current repository, and the subject
// under the former name each of them answers. The length the publisher declines a rewrite past is
// its own case (publisher_test.go), where the decline lives.
func TestRewrite(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		subject string
		want    string
	}{
		{
			name:    "the deployment's pair rewrites a pull request review",
			value:   "sjawhar/legion=cybermonklabs/legion",
			subject: "notifications.github.cybermonklabs.legion.pr.42.review",
			want:    "notifications.github.sjawhar.legion.pr.42.review",
		},
		{
			name:    "a name holding a dot is matched and written as a subject writes it",
			value:   "acme/old.site=acme/new.site",
			subject: "notifications.github.acme.new_site.pr.1",
			want:    "notifications.github.acme.old_site.pr.1",
		},
		{
			name:    "the second pair of the value rewrites too",
			value:   "sjawhar/legion=cybermonklabs/legion,acme/old=acme/new",
			subject: "notifications.github.acme.new.issue.7",
			want:    "notifications.github.acme.old.issue.7",
		},
		{
			name:    "GitHub's capitals in a subject name the configured repository",
			value:   "sjawhar/legion=cybermonklabs/legion",
			subject: "notifications.github.CyberMonkLabs.Legion.pr.42",
			want:    "notifications.github.sjawhar.legion.pr.42",
		},
		{
			name:    "a Slack subject names no repository",
			value:   "sjawhar/legion=cybermonklabs/legion",
			subject: "notifications.slack.T01.C02.thread.1700000000_1.message",
			want:    "",
		},
		{
			name:    "an agent subject names no repository",
			value:   "sjawhar/legion=cybermonklabs/legion",
			subject: "notifications.agent.session-7",
			want:    "",
		},
		{
			name:    "a repository no pair names",
			value:   "sjawhar/legion=cybermonklabs/legion",
			subject: "notifications.github.acme.other.pr.1",
			want:    "",
		},
		{
			name:    "the former name is never rewritten back",
			value:   "sjawhar/legion=cybermonklabs/legion",
			subject: "notifications.github.sjawhar.legion.pr.42",
			want:    "",
		},
		{
			name:    "the repository prefix with no fifth token",
			value:   "sjawhar/legion=cybermonklabs/legion",
			subject: "notifications.github.cybermonklabs.legion",
			want:    "",
		},
		{
			name:    "the repository prefix with a trailing dot and nothing after it",
			value:   "sjawhar/legion=cybermonklabs/legion",
			subject: "notifications.github.cybermonklabs.legion.",
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			renames, err := Parse(tc.value)
			if err != nil {
				t.Fatalf("Parse(%q) refused the value: %v, want the pairs it holds", tc.value, err)
			}
			got, _, rewritten := renames.rewrite(tc.subject)
			if got != tc.want || rewritten != (tc.want != "") {
				t.Errorf("rewrite(%q) = %q, %v, want %q, %v", tc.subject, got, rewritten, tc.want, tc.want != "")
			}
		})
	}
}

// TestZeroRenames covers the deployment that configures no rename: every subject is answered no,
// and nothing is held.
func TestZeroRenames(t *testing.T) {
	var zero Renames
	if !zero.IsZero() {
		t.Error("the zero Renames reports IsZero() = false, want true")
	}
	if pairs := zero.Pairs(); len(pairs) != 0 {
		t.Errorf("the zero Renames holds Pairs() = %v, want none", pairs)
	}
	for _, subject := range []string{
		"notifications.github.cybermonklabs.legion.pr.42.review",
		"notifications.github.sjawhar.legion.pr.42",
		"notifications.slack.T01.C02.message",
		"",
	} {
		if got, _, rewritten := zero.rewrite(subject); rewritten || got != "" {
			t.Errorf("the zero Renames answered rewrite(%q) = %q, %v, want %q, false", subject, got, rewritten, "")
		}
	}
}

// TestRenamesString covers the line the listener logs at start: each pair as the value spelled
// it, and the subject prefix that pair's copies go out under. The prefix is what tells an
// operator who wrote the former name in a case GitHub never used that the copies address a
// subject nothing subscribes to, since a NATS subject is compared with case.
func TestRenamesString(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "no pair", value: "", want: ""},
		{
			name:  "one pair",
			value: "sjawhar/legion=cybermonklabs/legion",
			want:  "sjawhar/legion=cybermonklabs/legion copies to notifications.github.sjawhar.legion",
		},
		{
			name:  "two pairs, in configured order and as the value spelled them",
			value: " acme/old = acme/new , sjawhar/legion=cybermonklabs/legion ",
			want: "acme/old=acme/new copies to notifications.github.acme.old, " +
				"sjawhar/legion=cybermonklabs/legion copies to notifications.github.sjawhar.legion",
		},
		{
			name:  "a former name holding a dot, as a subject writes it",
			value: "acme/old.site=acme/new",
			want:  "acme/old.site=acme/new copies to notifications.github.acme.old_site",
		},
		{
			name:  "a former name spelled with capitals keeps them, since a subject is compared with case",
			value: "SJawhar/Legion=cybermonklabs/legion",
			want:  "SJawhar/Legion=cybermonklabs/legion copies to notifications.github.SJawhar.Legion",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			renames, err := Parse(tc.value)
			if err != nil {
				t.Fatalf("Parse(%q) refused the value: %v, want the pairs it holds", tc.value, err)
			}
			if got := renames.String(); got != tc.want {
				t.Errorf("Parse(%q).String() = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}
