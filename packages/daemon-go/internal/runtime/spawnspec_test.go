package runtime

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

func spawnSpec() SpawnSpec {
	return SpawnSpec{
		Claim:      claim.Token("legion-omp-legion-43-tester"),
		Project:    "omp",
		Tree:       "LEGION-42",
		Issue:      "LEGION-43",
		Role:       claim.RoleTester,
		Generation: 2,
		BootToken:  "boot-secret",
		Env:        map[string]string{"JJ_USER": "legion-tester", "GH_CONFIG_DIR": "/state/gh"},
		Secrets:    map[string]string{"ENVOY_TOKEN": "envoy-secret"},
		Prompt:     PromptParts{RolePromptPaths: []string{"/roles/tester.md"}},
		Repository: "sjawhar/legion",
	}
}

// Every runtime refuses a spec it could not honour exactly, before anything touches its disk or its
// cluster — above all one that would put a variable the runtime owns, or a credential, into the
// agent's environment as a plain value.
func TestValidateSpawnSpecRefusesWhatNoRuntimeCouldHonour(t *testing.T) {
	owned := map[string]bool{"LEGION_TREE": true, "LEGION_BOOT_TOKEN_FILE": true}
	for _, tc := range []struct {
		name   string
		mutate func(*SpawnSpec)
		want   string
	}{
		{"no claim", func(s *SpawnSpec) { s.Claim = "" }, "spawn: no claim token"},
		{"no tree", func(s *SpawnSpec) { s.Tree = "" }, "spawn legion-omp-legion-43-tester: no tree"},
		{"no boot token", func(s *SpawnSpec) { s.BootToken = "" }, "spawn legion-omp-legion-43-tester: no boot token"},
		{"an issue that is a path", func(s *SpawnSpec) { s.Issue = "../../etc" }, `spawn legion-omp-legion-43-tester: issue "../../etc" is not an issue key`},
		{"a tree that is not an issue key", func(s *SpawnSpec) { s.Tree = "legion-42" }, `spawn legion-omp-legion-43-tester: tree "legion-42" is not an issue key`},
		{"a role no claim is on", func(s *SpawnSpec) { s.Role = "controller" }, `spawn legion-omp-legion-43-tester: "controller" is not a role`},
		{"no role prompt", func(s *SpawnSpec) { s.Prompt.RolePromptPaths = nil }, "spawn legion-omp-legion-43-tester: no role prompt"},
		{"an Env name that is not a variable name", func(s *SpawnSpec) { s.Env["BAD NAME"] = "x" }, `spawn legion-omp-legion-43-tester: Env name "BAD NAME" is not an environment variable name`},
		{"a variable the runtime sets", func(s *SpawnSpec) { s.Env["LEGION_TREE"] = "OTHER-1" }, "spawn legion-omp-legion-43-tester: Env sets LEGION_TREE, which the runtime sets itself"},
		{"a credential as a value", func(s *SpawnSpec) { s.Env["ANTHROPIC_API_KEY"] = "sk-x" }, "spawn legion-omp-legion-43-tester: Env carries ANTHROPIC_API_KEY, a credential-shaped name; a secret travels in Secrets, as a file"},
		{"a secret name that is not a variable name", func(s *SpawnSpec) { s.Secrets["BAD NAME"] = "x" }, `spawn legion-omp-legion-43-tester: secret "BAD NAME" is not an environment variable name`},
		{"a secret whose pointer the runtime owns", func(s *SpawnSpec) { s.Secrets["LEGION_BOOT_TOKEN"] = "x" }, "spawn legion-omp-legion-43-tester: secret LEGION_BOOT_TOKEN's pointer LEGION_BOOT_TOKEN_FILE is a variable the runtime sets itself"},
		{"a secret whose pointer Env also sets", func(s *SpawnSpec) { s.Env["ENVOY_TOKEN_FILE"] = "/elsewhere" }, "spawn legion-omp-legion-43-tester: secret ENVOY_TOKEN's pointer ENVOY_TOKEN_FILE is also set in Env"},
		{"a resume of a workspace recovered after its volume was lost", func(s *SpawnSpec) {
			s.ResumeSessionFile, s.WorkspaceRecoveredFrom = "/sessions/tester.jsonl", "legion/LEGION-43"
		}, "spawn legion-omp-legion-43-tester: ResumeSessionFile /sessions/tester.jsonl and WorkspaceRecoveredFrom legion/LEGION-43 are both set: a workspace recovered after its volume was lost holds no session to resume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := spawnSpec()
			tc.mutate(&spec)
			if err := ValidateSpawnSpec(spec, owned); err == nil || err.Error() != tc.want {
				t.Fatalf("ValidateSpawnSpec = %v, want %q", err, tc.want)
			}
		})
	}
	for name, mutate := range map[string]func(*SpawnSpec){
		"as it is":                    func(*SpawnSpec) {},
		"with no repository":          func(s *SpawnSpec) { s.Repository = "" },
		"with a credential's pointer": func(s *SpawnSpec) { s.Env["GH_TOKEN_FILE"] = "/state/gh-token" },
		"resuming a session":          func(s *SpawnSpec) { s.ResumeSessionFile = "/sessions/tester.jsonl" },
		"recovering a lost workspace": func(s *SpawnSpec) { s.WorkspaceRecoveredFrom = "legion/LEGION-43" },
	} {
		spec := spawnSpec()
		mutate(&spec)
		if err := ValidateSpawnSpec(spec, owned); err != nil {
			t.Errorf("a well-formed spec %s was refused: %v", name, err)
		}
	}
}
