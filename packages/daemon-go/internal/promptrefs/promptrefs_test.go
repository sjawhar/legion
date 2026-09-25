package promptrefs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// AddEncoded reads back exactly what Roles writes, a kind with no names included; anything else is
// refused naming what is wrong, so a daemon and an image that disagree on the encoding refuse the
// probe instead of resolving no role prompt at all.
func TestAddEncodedReadsRolesEncodingAndRefusesAnyOther(t *testing.T) {
	roles := t.TempDir()
	if err := os.WriteFile(filepath.Join(roles, "architect.md"), []byte("Load skill://dispatch first."), 0o644); err != nil {
		t.Fatal(err)
	}
	encoded, err := Roles(roles)
	if err != nil {
		t.Fatal(err)
	}
	names := New()
	if err := names.AddEncoded(encoded); err != nil {
		t.Fatalf("AddEncoded(Roles' own %s) = %v, want it read", encoded, err)
	}
	if got := names[Skills]["dispatch"]; !slices.Equal(got, []string{"roles/architect.md"}) {
		t.Errorf("AddEncoded(%s) read skill dispatch as named by %q, want roles/architect.md", encoded, got)
	}

	for _, testCase := range []struct{ name, raw, want string }{
		{"not JSON", `LEGION_PROMPT_AGENTS=oracle`, "not promptrefs.Roles' encoding"},
		{"null", `null`, "no LEGION_PROMPT_AGENTS"},
		{"an empty object", `{}`, "no LEGION_PROMPT_AGENTS"},
		{"a kind missing", `{"LEGION_PROMPT_AGENTS":{}}`, "no LEGION_PROMPT_SKILLS"},
		{"a kind that is null", `{"LEGION_PROMPT_AGENTS":null,"LEGION_PROMPT_SKILLS":{}}`, "no LEGION_PROMPT_AGENTS"},
		{"a misspelled kind", `{"LEGION_PROMPT_AGENTS":{},"LEGION_PROMPT_SKILLS":{},"LEGION_PROMPT_SKILL":{"dispatch":["roles/architect.md"]}}`, "unknown kind LEGION_PROMPT_SKILL"},
		{"a name no file names", `{"LEGION_PROMPT_AGENTS":{"oracle":[]},"LEGION_PROMPT_SKILLS":{}}`, "LEGION_PROMPT_AGENTS name oracle is named by no file"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := New().AddEncoded(testCase.raw)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("AddEncoded(%s) = %v, want a refusal saying %q", testCase.raw, err, testCase.want)
			}
		})
	}
}
