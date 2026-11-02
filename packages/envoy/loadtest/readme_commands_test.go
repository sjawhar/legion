package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestREADMEBashCommandsRunAsWritten: every command in a ```bash block of README.md names a
// program the reader can run from the repository root, where the README's own "Run one step"
// examples are run. Nothing of this package is on a reader's PATH: `stack.sh replay-seed ...` and
// `loadgen replay-plan ...` both answer "command not found", so a block that names one documents a
// command that does not run. A script is named by its path under packages/envoy/loadtest/, and
// loadgen is reached as `stack.sh loadgen ...` or as "$LOADTEST_ROOT/bin/loadgen".
func TestREADMEBashCommandsRunAsWritten(t *testing.T) {
	const scriptDir = "packages/envoy/loadtest/"
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	inBlock, continued := false, false
	for i, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "```bash"):
			inBlock, continued = true, false
			continue
		case strings.HasPrefix(line, "```"):
			inBlock = false
			continue
		case !inBlock:
			continue
		}
		cmd := strings.TrimSpace(line)
		wasContinued := continued
		continued = strings.HasSuffix(cmd, "\\")
		if wasContinued || cmd == "" || strings.HasPrefix(cmd, "#") {
			continue
		}
		word, _, _ := strings.Cut(cmd, " ")
		switch {
		case strings.HasSuffix(word, ".sh") && !strings.HasPrefix(word, scriptDir):
			bad = append(bad, fmt.Sprintf("README.md:%d: %q names a script that is not on a reader's PATH; write %s%s", i+1, word, scriptDir, word))
		case word == "loadgen":
			bad = append(bad, fmt.Sprintf("README.md:%d: %q is not on a reader's PATH; run it as `%sstack.sh loadgen ...` or as \"$LOADTEST_ROOT/bin/loadgen\"", i+1, word, scriptDir))
		}
	}
	if len(bad) > 0 {
		t.Fatalf("commands that do not run as written:\n%s", strings.Join(bad, "\n"))
	}
}
