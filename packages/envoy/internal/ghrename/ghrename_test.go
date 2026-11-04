package ghrename

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
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

func TestFormerSubject(t *testing.T) {
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
			got, rewritten := renames.FormerSubject(tc.subject)
			if got != tc.want || rewritten != (tc.want != "") {
				t.Errorf("FormerSubject(%q) = %q, %v, want %q, %v", tc.subject, got, rewritten, tc.want, tc.want != "")
			}
		})
	}
}

// TestFormerSubjectSubjectMaxBytes covers the one rewrite that is declined rather than published:
// a former name longer than the current one makes a subject the bus would refuse for ever, while
// the publish it copies succeeds.
func TestFormerSubjectSubjectMaxBytes(t *testing.T) {
	renames, err := Parse("acme/a-much-longer-former-name=acme/new")
	if err != nil {
		t.Fatalf("Parse refused the pair: %v, want the pair it holds", err)
	}
	const current = "notifications.github.acme.new"
	const former = "notifications.github.acme.a-much-longer-former-name"

	cases := []struct {
		name string
		// length is the length of the rewritten subject the trailing segment is built for.
		length int
		want   bool
	}{
		{name: "a rewrite of exactly SubjectMaxBytes", length: contracts.SubjectMaxBytes, want: true},
		{name: "a rewrite one byte past SubjectMaxBytes", length: contracts.SubjectMaxBytes + 1, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail := strings.Repeat("a", tc.length-len(former)-1)
			subject := current + "." + tail
			want := former + "." + tail
			got, rewritten := renames.FormerSubject(subject)
			if rewritten != tc.want {
				t.Fatalf("FormerSubject of a subject rewriting to %d bytes answered %v, want %v (SubjectMaxBytes is %d)",
					tc.length, rewritten, tc.want, contracts.SubjectMaxBytes)
			}
			if !tc.want {
				if got != "" {
					t.Errorf("a declined rewrite answered %.60q… , want the empty subject", got)
				}
				return
			}
			if got != want || len(got) != contracts.SubjectMaxBytes {
				t.Errorf("a rewrite of %d bytes = %.60q… (%d bytes), want the subject under the former name at %d bytes",
					tc.length, got, len(got), contracts.SubjectMaxBytes)
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
		if got, rewritten := zero.FormerSubject(subject); rewritten || got != "" {
			t.Errorf("the zero Renames answered FormerSubject(%q) = %q, %v, want %q, false", subject, got, rewritten, "")
		}
	}
}
