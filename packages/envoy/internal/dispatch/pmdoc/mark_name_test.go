package pmdoc

import (
	"regexp"
	"testing"
)

// yattrMarkSuffixOracle is the pattern a record mark's Y.Text attribute key was read by before
// yattrToMarkName checked its bytes: a type, `--` and 8 standard base64 characters.
var yattrMarkSuffixOracle = regexp.MustCompile(`^(.*)--[a-zA-Z0-9+/=]{8}$`)

func TestYattrToMarkNameDropsARecordMarksDigest(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{name: "proofComment--AbCd12+/", want: "proofComment"},
		{name: "proofComment", want: "proofComment"},
		{name: "proofComment--AbCd12+/9", want: "proofComment--AbCd12+/9"},
		{name: "proofComment--AbCd12+", want: "proofComment--AbCd12+"},
		{name: "proofComment--AbCd12+-", want: "proofComment--AbCd12+-"},
		{name: "proof\nComment--AbCd12+/", want: "proof\nComment--AbCd12+/"},
		{name: "--abcdefgh", want: ""},
		{name: "a--b--abcdefgh", want: "a--b"},
		{name: "commentaire\u00e9\U0001F600--abcd==/+", want: "commentaire\u00e9\U0001F600"},
		{name: "x--abc=efgh", want: "x"},
		{name: "\xff--abcdefgh", want: "\xff"},
		{name: "", want: ""},
	} {
		if got := yattrToMarkName(test.name); got != test.want {
			t.Errorf("yattrToMarkName(%q) = %q, want %q", test.name, got, test.want)
		}
		if oracle := yattrMarkSuffixOracle.ReplaceAllString(test.name, "$1"); oracle != test.want {
			t.Errorf("the pattern reads %q as %q, the table wants %q", test.name, oracle, test.want)
		}
	}
}

// Whatever the key, yattrToMarkName reads it as the pattern it replaced did.
func FuzzYattrToMarkName(f *testing.F) {
	for _, seed := range []string{"proofComment--AbCd12+/", "a--b--abcdefgh", "x\n--abcdefgh", "--abcdefg", "\xff\xfe--ABCDEFGH", "strong"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if got, want := yattrToMarkName(name), yattrMarkSuffixOracle.ReplaceAllString(name, "$1"); got != want {
			t.Fatalf("yattrToMarkName(%q) = %q, the pattern reads %q", name, got, want)
		}
	})
}
