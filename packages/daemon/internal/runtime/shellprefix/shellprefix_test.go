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
	got := For("/legion/worker-bin", "/opt/legion/bin")
	want := `__legion_path=:${PATH//:/::}: && ` +
		`__legion_path=${__legion_path//:'/legion/worker-bin':/} && ` +
		`__legion_path=${__legion_path//:'/opt/legion/bin':/} && ` +
		`__legion_path=${__legion_path//::/:} && __legion_path=${__legion_path#:} && ` +
		`PATH='/legion/worker-bin:/opt/legion/bin'${__legion_path:+:${__legion_path%:}} && unset __legion_path &&`
	if got != want {
		t.Fatalf("For = %q\nwant  %q", got, want)
	}
}

// A directory with a quote in it stays one literal word in every step that names it.
func TestForQuotesADirectoryWithAQuoteInIt(t *testing.T) {
	got := For("/state/it's", "/bin dir")
	want := `__legion_path=:${PATH//:/::}: && ` +
		`__legion_path=${__legion_path//:'/state/it'\''s':/} && ` +
		`__legion_path=${__legion_path//:'/bin dir':/} && ` +
		`__legion_path=${__legion_path//::/:} && __legion_path=${__legion_path#:} && ` +
		`PATH='/state/it'\''s:/bin dir'${__legion_path:+:${__legion_path%:}} && unset __legion_path &&`
	if got != want {
		t.Fatalf("For = %q\nwant  %q", got, want)
	}
}

// runPrefix runs prefix in bash under each PATH given, then prints the PATH it left and whether
// the helper variable is gone.
func runPrefix(t *testing.T, prefix string, paths ...string) []string {
	t.Helper()
	var script strings.Builder
	for _, path := range paths {
		script.WriteString("PATH=" + Literal(path) + "\n" + prefix + ` printf '%s\n' "$PATH" "${__legion_path-unset}"` + "\n")
	}
	out, err := exec.Command("bash", "-c", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v: %s", err, out)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// Wherever the rc left the directories, and however many times, the prefix leaves each first and
// once, in order, and every other entry in its own order, empty entries (the working directory)
// included. The devbox's ~/.bashrc puts its dotfiles shims first, ahead of a worker-bin the pane
// already had.
func TestForPutsEachDirectoryFirstAndOnceWhereverTheRcLeftIt(t *testing.T) {
	const workerBin, bin = "/state/worker bin", "/state/it's bin"
	prefix := For(workerBin, bin)
	head := workerBin + ":" + bin
	for _, tc := range []struct{ name, path, want string }{
		{"already in front", head + ":/usr/bin:/bin", head + ":/usr/bin:/bin"},
		{"shims first, worker-bin in the middle", "/home/u/.dotfiles/shims:" + workerBin + ":/usr/bin:/bin", head + ":/home/u/.dotfiles/shims:/usr/bin:/bin"},
		{"shims first, both in front of the rest and again at the end", "/home/u/.dotfiles/shims:" + head + ":/usr/bin:" + workerBin + ":" + workerBin, head + ":/home/u/.dotfiles/shims:/usr/bin"},
		{"reversed and repeated", bin + ":" + bin + ":/usr/bin:" + workerBin, head + ":/usr/bin"},
		{"a prefix of a directory is another directory", workerBin + "s:/usr/bin:" + workerBin + "/sub", head + ":" + workerBin + "s:/usr/bin:" + workerBin + "/sub"},
		{"empty entries keep their places", ":/usr/bin::" + workerBin + ":", head + "::/usr/bin::"},
		{"nothing but the directories", workerBin + ":" + bin + ":" + workerBin, head},
		{"empty", "", head},
	} {
		got := runPrefix(t, prefix, tc.path)
		if got[0] != tc.want || got[1] != "unset" {
			t.Errorf("%s: PATH %q became %q (helper %q), want %q with the helper unset", tc.name, tc.path, got[0], got[1], tc.want)
		}
	}
}

// Oh My Pi's bash tool sources the operator's rc file and replays the PATH it leaves, so an rc that
// prepends its own directories lands them ahead of the agent's. Run before a command, the prefix
// puts the given directories back in front, and running it again leaves PATH exactly as it was.
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

// Word leaves a word of the safe set bare and single-quotes anything else, closing and
// reopening the quote around a `'` (runtime.ts:327-329).
func TestWord(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/usr/local/bin/omp", "/usr/local/bin/omp"},
		{"github:sjawhar/oh-my-pi@18", "'github:sjawhar/oh-my-pi@18'"},
		{"/state dir/worker-bin:/usr/bin", "'/state dir/worker-bin:/usr/bin'"},
		{"it's", `'it'\''s'`},
		{"$HOME", "'$HOME'"},
	} {
		if got := Word(tc.in); got != tc.want {
			t.Errorf("Word(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
