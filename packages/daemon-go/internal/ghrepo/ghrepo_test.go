package ghrepo

import (
	"reflect"
	"strings"
	"testing"
)

// Parse holds every input to one rule: exactly `<owner>/<name>`, both non-empty, no whitespace,
// no `.` or `..` segment, and GitHub's own grammar for each name, and each refusal names the input
// and the value. The names GitHub allows parse, dotted and underscored ones included.
func TestParse(t *testing.T) {
	repository, err := Parse("--repo", "acme/widgets")
	if err != nil || repository.Owner() != "acme" || repository.Name() != "widgets" || repository.String() != "acme/widgets" {
		t.Fatalf(`Parse("acme/widgets") = %#v (%s), %v, want acme, widgets`, repository, repository, err)
	}
	valid := []string{"sjawhar/.github", "sjawhar/legion-smoke", "my-org/a_b.c", "A1/-lead", "a/b..c", "o/...",
		// Owners GitHub serves that its sign-up form would refuse today.
		"ArtOfCode-/APiPy", "hello--world/a", "foo--bar/arthanaya", "mona-cat_octo/scratch",
		// GitHub's limits exactly: a 39-character owner, a 100-character name.
		strings.Repeat("o", 39) + "/b", "a/" + strings.Repeat("n", 100),
	}
	for _, valid := range valid {
		if parsed, err := Parse("--repo", valid); err != nil || parsed.String() != valid {
			t.Errorf("Parse(%q) = %s, %v, want it read as written", valid, parsed, err)
		}
	}
	const owner = ` has an owner GitHub does not allow: at most 39 ASCII letters, digits, "-" and "_", beginning with a letter or digit`
	const name = ` has a name GitHub does not allow: at most 100 ASCII letters, digits, "-", "_" and "."`
	long := strings.Repeat("o", 40) + "/b"
	longer := "a/" + strings.Repeat("n", 101)
	for _, tc := range []struct{ repository, want string }{
		{"", `--repo must be "owner/name" (got "")`},
		{"acme", `--repo must be "owner/name" (got "acme")`},
		{"/widgets", `--repo must be "owner/name" (got "/widgets")`},
		{"acme/", `--repo must be "owner/name" (got "acme/")`},
		{"acme/widgets/extra", `--repo must be "owner/name" (got "acme/widgets/extra")`},
		{"acme/wid gets", `--repo "acme/wid gets" holds whitespace, which no GitHub owner or repository name does`},
		{"acme\t/widgets", `--repo "acme\t/widgets" holds whitespace, which no GitHub owner or repository name does`},
		{"./widgets", `--repo "./widgets" has a "." segment, which names no GitHub owner or repository`},
		{"acme/..", `--repo "acme/.." has a ".." segment, which names no GitHub owner or repository`},
		// Control bytes, which a log line or a terminal would act on.
		{"a/\x00b", `--repo "a/\x00b"` + name},
		{"a/\x1b[31m", `--repo "a/\x1b[31m"` + name},
		{"\x7fa/b", `--repo "\x7fa/b"` + owner},
		// URL metacharacters, which end the path of the URL a clone names.
		{"a/b#frag", `--repo "a/b#frag"` + name},
		{"a/b?x", `--repo "a/b?x"` + name},
		// A percent escape, which a URL decodes into another path.
		{"a%2F/b", `--repo "a%2F/b"` + owner},
		// A hyphen or an underscore beginning an owner, which no login does.
		{"-a/b", `--repo "-a/b"` + owner},
		{"_a/b", `--repo "_a/b"` + owner},
		// Past GitHub's limits: a 40-character owner, a 101-character name.
		{long, `--repo "` + long + `"` + owner},
		{longer, `--repo "` + longer + `"` + name},
		// NATS wildcards, which in intake's GitHub filter would match other repositories.
		{"a/*", `--repo "a/*"` + name},
		{"a/>", `--repo "a/>"` + name},
		{"*/b", `--repo "*/b"` + owner},
		{">/b", `--repo ">/b"` + owner},
		// Dots in an owner, which GitHub allows only in a repository's name.
		{".acme/b", `--repo ".acme/b"` + owner},
		// A lookalike letter, printed escaped beside the rule it breaks.
		{"\u0430cme/widgets", `--repo "\u0430cme/widgets"` + owner},
		{"acme/wid\u0433ets", `--repo "acme/wid\u0433ets"` + name},
		// A name ending in ".git", which GitHub strips at creation: its API answers 404 for the
		// suffixed name, and its events name the repository without it.
		{"acme/widgets.git", `--repo "acme/widgets.git" ends in ".git", which GitHub strips from a repository's name: name it "acme/widgets"`},
		{"a/.git", `--repo "a/.git" ends in ".git", which GitHub strips from a repository's name: name it "a/"`},
	} {
		if _, err := Parse("--repo", tc.repository); err == nil || err.Error() != tc.want {
			t.Errorf("Parse(%q) = %v, want %q", tc.repository, err, tc.want)
		}
	}
}

// Only Parse makes a non-zero Repository: the type exports no field, so no caller outside this
// package builds one Parse would refuse, whose names, joined under a state directory, would name
// another directory than the repository's (provisioning removes an incomplete clone there). The
// zero value is the one other Repository, and IsZero names it.
func TestRepositoryExportsNoField(t *testing.T) {
	repository := reflect.TypeFor[Repository]()
	for i := range repository.NumField() {
		if field := repository.Field(i); field.IsExported() {
			t.Errorf("Repository exports its field %s: any caller can build a Repository Parse would refuse", field.Name)
		}
	}
	if !(Repository{}).IsZero() || MustParse("acme/widgets").IsZero() {
		t.Errorf("IsZero: the zero Repository must be zero and a parsed one must not")
	}
}
