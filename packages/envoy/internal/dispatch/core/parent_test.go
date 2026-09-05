package core

import "testing"

func TestParseParentIssueOnly(t *testing.T) {
	p, err := ParseParent("642")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.IssueNumber != 642 || p.CommentID != 0 {
		t.Errorf("got %+v", p)
	}
}

func TestParseParentWithComment(t *testing.T) {
	p, err := ParseParent("642#3216548790")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p.IssueNumber != 642 || p.CommentID != 3216548790 {
		t.Errorf("got %+v", p)
	}
}

func TestParseParentQualified(t *testing.T) {
	cases := []struct {
		input   string
		repo    string
		issue   int
		comment int
	}{
		{"acme/widgets#42", "acme/widgets", 42, 0},
		{"acme/widgets#42#3216548790", "acme/widgets", 42, 3216548790},
		{"  acme/widgets#42  ", "acme/widgets", 42, 0},
	}
	for _, tc := range cases {
		p, err := ParseParent(tc.input)
		if err != nil {
			t.Errorf("%q: err: %v", tc.input, err)
			continue
		}
		if p.Repo != tc.repo || p.IssueNumber != tc.issue || p.CommentID != tc.comment {
			t.Errorf("%q: got %+v", tc.input, p)
		}
	}
}

func TestParseParentInvalid(t *testing.T) {
	for _, input := range []string{
		"abc", "0", "642#", "642#abc", "642#0", "", "1#2#3",
		"acme/widgets", "acme/widgets#", "acme/widgets#0", "acme/widgets#42#", "acme/widgets#42#0",
		"acme/widgets#42#7#9", "acme/wid gets#42", "acme/a/b#42", "/widgets#42", "acme/#42",
	} {
		if _, err := ParseParent(input); err == nil {
			t.Errorf("expected error for %q", input)
		}
	}
}

func TestParseThread(t *testing.T) {
	cases := []struct {
		in      string
		repo    string
		number  int
		wantErr string
	}{
		{"42", "", 42, ""},
		{" acme-org/example-repo#17158 ", "acme-org/example-repo", 17158, ""},
		{"", "", 0, "Invalid thread: "},
		{"abc", "", 0, "Invalid thread: abc"},
		{"42#900", "", 0, "Invalid thread: 42#900"},
		{"acme-org/example-repo#42#900", "", 0, "Invalid thread: acme-org/example-repo#42#900"},
		{"0", "", 0, "Invalid thread: 0"},
		{"042", "", 0, "Invalid thread: 042"},
		{"acme-org/example-repo#0", "", 0, "Invalid thread: acme-org/example-repo#0"},
	}
	for _, tc := range cases {
		got, err := ParseThread(tc.in)
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("ParseThread(%q): err %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseThread(%q): %v", tc.in, err)
			continue
		}
		if got.Repo != tc.repo || got.IssueNumber != tc.number {
			t.Errorf("ParseThread(%q) = %+v", tc.in, got)
		}
	}
}
