package text

import "testing"

func TestStorableReplacement(t *testing.T) {
	for input, want := range map[string]string{
		"clean":       "clean",
		"a\x00b":      "a\uFFFDb",
		"a\xffb\x00c": "a\uFFFDb\uFFFDc",
	} {
		if got := StorableReplacement(input); got != want {
			t.Errorf("StorableReplacement(%q) = %q, want %q", input, got, want)
		} else if !Storable(got) {
			t.Errorf("StorableReplacement(%q) = %q, which is not storable", input, got)
		}
	}
}
