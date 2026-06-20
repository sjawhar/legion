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

func TestParseParentInvalid(t *testing.T) {
	for _, input := range []string{"abc", "0", "642#", "642#abc", "642#0", "", "1#2#3"} {
		if _, err := ParseParent(input); err == nil {
			t.Errorf("expected error for %q", input)
		}
	}
}
