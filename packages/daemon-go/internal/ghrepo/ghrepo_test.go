package ghrepo

import (
	"reflect"
	"testing"
)

// Parse holds every input to one rule: exactly `<owner>/<name>`, both non-empty, no whitespace,
// and no `.` or `..` segment, and each refusal names the input and the value.
func TestParse(t *testing.T) {
	repository, err := Parse("--repo", "acme/widgets")
	if err != nil || repository.Owner() != "acme" || repository.Name() != "widgets" || repository.String() != "acme/widgets" {
		t.Fatalf(`Parse("acme/widgets") = %#v (%s), %v, want acme, widgets`, repository, repository, err)
	}
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
