package ghrepo

import "testing"

// Split holds every input to one rule: exactly `<owner>/<name>`, both non-empty, no whitespace,
// and no `.` or `..` segment, and each refusal names the input and the value.
func TestSplit(t *testing.T) {
	owner, name, err := Split("--repo", "acme/widgets")
	if err != nil || owner != "acme" || name != "widgets" {
		t.Fatalf(`Split("acme/widgets") = %q, %q, %v, want acme, widgets`, owner, name, err)
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
		if _, _, err := Split("--repo", tc.repository); err == nil || err.Error() != tc.want {
			t.Errorf("Split(%q) = %v, want %q", tc.repository, err, tc.want)
		}
	}
}
