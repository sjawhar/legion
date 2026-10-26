package ghtool

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The stand-in gh is this test binary, linked as `gh` into a directory of its own (standInGH): run
// under that name it answers as gh, so the environment it records is exactly the one the tool gave
// it, with no shell between. It records every run in invocations.jsonl beside it, answers `<path>
// --help` with gh 2.98.0's help of that command, and answers every other run as behaviour.json
// beside it says.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "gh" {
		os.Exit(standIn())
	}
	os.Exit(m.Run())
}

// behaviour is how the stand-in answers a run: its stdout (or the response for its exact arguments,
// joined by spaces), its stderr, how long it sleeps first and its exit status; HelpDir is the saved
// help it answers --help from.
type behaviour struct {
	Stdout    string
	Stderr    string
	Responses map[string]string
	Sleep     time.Duration
	Exit      int
	HelpDir   string
}

// invocation is one run of the stand-in: its arguments, and for a run that is no help read, its
// whole environment, its working directory, and which of HOME, the XDG directories and the working
// directory were empty.
type invocation struct {
	Help  bool
	Argv  []string
	Env   []string
	Cwd   string
	Empty map[string]bool
}

func standIn() int {
	dir := filepath.Dir(os.Args[0])
	data, err := os.ReadFile(filepath.Join(dir, "behaviour.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "stand-in gh:", err)
		return 101
	}
	var b behaviour
	if err := json.Unmarshal(data, &b); err != nil {
		fmt.Fprintln(os.Stderr, "stand-in gh:", err)
		return 101
	}
	args := os.Args[1:]
	if n := len(args); n > 0 && args[n-1] == "--help" {
		record(dir, invocation{Help: true, Argv: args})
		text, err := standInHelp(b.HelpDir, args[:n-1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Print(text)
		return 0
	}
	cwd, _ := os.Getwd()
	inv := invocation{Argv: args, Env: slices.Sorted(slices.Values(os.Environ())), Cwd: cwd, Empty: map[string]bool{}}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		inv.Empty[name] = isEmptyDir(os.Getenv(name))
	}
	inv.Empty["cwd"] = isEmptyDir(cwd)
	record(dir, inv)
	time.Sleep(b.Sleep)
	out := b.Stdout
	if response, ok := b.Responses[strings.Join(args, " ")]; ok {
		out = response
	}
	os.Stdout.WriteString(out)
	os.Stderr.WriteString(b.Stderr)
	return b.Exit
}

func isEmptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

func record(dir string, inv invocation) {
	line, _ := json.Marshal(inv)
	file, err := os.OpenFile(filepath.Join(dir, "invocations.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(line, '\n'))
}

// standInHelp is gh 2.98.0's help of path: the saved help of that command when there is one (gh's
// own for none), and otherwise the help gh prints, rebuilt from its saved reference: a group's
// usage and its commands, or a leaf's usage, its options and the options it inherits (--help, and
// -R for the commands of a group that reads a repository). A command neither knows is gh's
// `unknown command`.
func standInHelp(helpDir string, path []string) (string, error) {
	name := "gh"
	if len(path) > 0 {
		name = strings.Join(path, "_")
	}
	if data, err := os.ReadFile(filepath.Join(helpDir, name+".txt")); err == nil {
		return string(data), nil
	}
	reference, err := os.Open(filepath.Join(helpDir, "reference.txt"))
	if err != nil {
		return "", err
	}
	defer reference.Close()
	spelled := "gh " + strings.Join(path, " ")
	var usage string
	var flags, children []string
	inside := false
	scanner := bufio.NewScanner(reference)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			heading := strings.Trim(strings.TrimSpace(strings.TrimLeft(line, "#")), "`")
			inside = false
			rest, ok := strings.CutPrefix(heading, spelled)
			if !ok || (rest != "" && rest[0] != ' ') {
				continue
			}
			if words := strings.Fields(rest); len(words) > 0 && referenceCommandWord.MatchString(words[0]) {
				children = append(children, words[0])
				continue
			}
			usage, inside = heading, true
			continue
		}
		if inside && referenceFlagLine.MatchString(line) {
			flags = append(flags, line)
		}
	}
	if usage == "" {
		return "", fmt.Errorf("unknown command %q for %q", path[len(path)-1], "gh "+strings.Join(path[:len(path)-1], " "))
	}
	if len(children) > 0 {
		text := "USAGE\n  " + spelled + " <command> [flags]\n\nAVAILABLE COMMANDS\n"
		for _, child := range children {
			text += "  " + child + ":  " + child + "\n"
		}
		return text, nil
	}
	text := "USAGE\n  " + usage + "\n\nFLAGS\n" + strings.Join(flags, "\n") + "\n\nINHERITED FLAGS\n      --help                     Show help for command\n"
	if slices.Contains([]string{"pr", "issue", "release", "run", "workflow", "label", "cache"}, path[0]) {
		text += "  -R, --repo [HOST/]OWNER/REPO   Select another repository using the [HOST/]OWNER/REPO format\n"
	}
	return text, nil
}

// referenceCommandWord is a word of a command path in gh's reference; referenceFlagLine one of its
// option lines (internal/ghroute's ghCommandWord and ghFlagLine).
var (
	referenceCommandWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	referenceFlagLine    = regexp.MustCompile(`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S+))?(?:\s{2,}(.*)|\s*)$`)
)
