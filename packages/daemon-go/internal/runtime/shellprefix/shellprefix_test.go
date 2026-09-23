package shellprefix

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The exact bytes: Oh My Pi runs the prefix verbatim before every command, so a change in quoting
// or in the trailing `&&` is a change in what every agent's shell does.
func TestForIsTheAssignmentThatPutsTheDirectoriesFirst(t *testing.T) {
	got := For("/legion/worker-bin", "/opt/legion/go/bin")
	want := `PATH='/legion/worker-bin:/opt/legion/go/bin:'${PATH#'/legion/worker-bin:/opt/legion/go/bin:'} &&`
	if got != want {
		t.Fatalf("For = %q\nwant  %q", got, want)
	}
}

// A directory with a quote in it stays one literal word on both sides of the assignment.
func TestForQuotesADirectoryWithAQuoteInIt(t *testing.T) {
	got := For("/state/it's", "/bin dir")
	want := `PATH='/state/it'\''s:/bin dir:'${PATH#'/state/it'\''s:/bin dir:'} &&`
	if got != want {
		t.Fatalf("For = %q\nwant  %q", got, want)
	}
}

// Oh My Pi's bash tool sources the operator's rc file and replays the PATH it leaves, so an rc that
// prepends its own directories lands them ahead of the agent's. Run before a command, the prefix
// puts the given directories back in front — removing the copy an earlier command put there — and
// running it again leaves PATH exactly as it was.
func TestThePrefixResolvesItsDirectoriesAheadOfAnRcsAndIsStable(t *testing.T) {
	first, second, rc := filepath.Join(t.TempDir(), "worker bin"), filepath.Join(t.TempDir(), "it's bin"), t.TempDir()
	for dir, name := range map[string]string{first: "gh", second: "legion"} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rc, name), []byte("#!/bin/sh\nexit 97\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	prefix := For(first, second)
	script := "PATH=" + Literal(strings.Join([]string{rc, first, second, "/usr/bin", "/bin"}, ":")) + "\n" +
		prefix + " command -v gh\n" +
		prefix + " command -v legion\n" +
		"before=$PATH\n" +
		prefix + ` test "$PATH" = "$before" && echo stable`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v: %s", err, out)
	}
	want := filepath.Join(first, "gh") + "\n" + filepath.Join(second, "legion") + "\nstable\n"
	if string(out) != want {
		t.Fatalf("resolved\n%s\nwant\n%s", out, want)
	}
}
