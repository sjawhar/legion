package contracts

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Git bounds no ref's length, and the bus refuses a subject past SubjectMaxBytes, which a push's
// subject reaches with a ref of about 3,850 bytes: GitHub would be answered a refusal for that push
// on every delivery. A subject within the bound names its ref whole; past it, the ref's segment is
// cut to what fits and ends in `~` and 16 hex digits of the whole ref's SHA-256, so the subject is
// at most SubjectMaxBytes, a multibyte rune is never split, and refs that share the kept text, or
// differ only where the sanitizer writes `.` and `_` alike, keep subjects of their own. The
// expected subjects are the ones githubPushSubject in packages/contracts builds (envelope.test.ts).
func TestGithubPushSubjectCutsARefPastTheSubjectBound(t *testing.T) {
	const prefix = "notifications.github.acme.widgets.push.branch."
	room := SubjectMaxBytes - len(prefix)

	atBound := strings.Repeat("a", room)
	if got := GithubPushSubject("acme", "widgets", "branch", atBound); got != prefix+atBound {
		t.Errorf("a subject of exactly %d bytes = %.80q… (%d bytes), want its ref whole", SubjectMaxBytes, got, len(got))
	}

	oneOver := strings.Repeat("a", room+1)
	cut := GithubPushSubject("acme", "widgets", "branch", oneOver)
	if cut == prefix+oneOver || len(cut) > SubjectMaxBytes || !strings.HasPrefix(cut, prefix+strings.Repeat("a", room-17)+"~") {
		t.Errorf("a subject one byte past %d = %.80q… (%d bytes), want the ref cut to fit with ~ and its digest", SubjectMaxBytes, cut, len(cut))
	}

	long := strings.Repeat("a", 5000)
	want := prefix + strings.Repeat("a", room-17) + "~c526c6222044dab5"
	if got := GithubPushSubject("acme", "widgets", "branch", long); got != want {
		t.Errorf("a 5,000-byte ref's subject = …%q (%d bytes), want …%q (%d bytes)", tail(got), len(got), tail(want), len(want))
	}
	other := strings.Repeat("a", 4999) + "b"
	if got := GithubPushSubject("acme", "widgets", "branch", other); !strings.HasSuffix(got, "~dedc26db66630d52") || got == want {
		t.Errorf("a 5,000-byte ref differing from another only in its last byte has subject …%q, want its own digest", tail(got))
	}

	// Distinct refs the sanitizer writes alike: the digest is of the ref, not of its segment.
	dotted := GithubPushSubject("acme", "widgets", "branch", strings.Repeat("a.", 2500))
	underscored := GithubPushSubject("acme", "widgets", "branch", strings.Repeat("a_", 2500))
	if dotted == underscored {
		t.Errorf("two refs differing only in . and _ share the subject …%q", tail(dotted))
	}

	// 4-byte runes, under the longest owner (39 characters) and repository (100) GitHub allows: the
	// cut keeps as many whole runes as fit, so the subject is valid UTF-8 and within 3 bytes of the
	// bound.
	owner, repo := strings.Repeat("o", 39), strings.Repeat("r", 100)
	runePrefix := GithubSubject(owner, repo, "push.tag.")
	runes := GithubPushSubject(owner, repo, "tag", strings.Repeat("𝔸", 1250))
	if len(runes) > SubjectMaxBytes || len(runes) < SubjectMaxBytes-3 || !utf8.ValidString(runes) ||
		!strings.HasPrefix(runes, runePrefix+strings.Repeat("𝔸", (SubjectMaxBytes-len(runePrefix)-17)/4)+"~") {
		t.Errorf("a 5,000-byte ref of 4-byte runes has subject …%q (%d bytes, valid UTF-8 %v), want at most %d bytes of whole runes",
			tail(runes), len(runes), utf8.ValidString(runes), SubjectMaxBytes)
	}
}

// tail is the end of a subject, where a cut shows.
func tail(subject string) string {
	return subject[max(len(subject)-40, 0):]
}
