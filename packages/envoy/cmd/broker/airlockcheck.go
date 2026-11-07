package main

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/airlock"
)

// airlockCheckCommand is the argument that reads an airlock rules file instead of serving.
const airlockCheckCommand = "airlock-check"

// airlockCheckUsage is the one line every usage error of the subcommand prints, in the form the
// end-sessions usage line takes.
const airlockCheckUsage = "broker: usage: broker " + airlockCheckCommand +
	" <file> [--against <old file>], one rules file and nothing else"

// airlockCheck reads the airlock rules file args names and says whether the broker would serve
// it, with the broker's own airlock package and no second reader of the format: airlock.Parse,
// which refuses a field the broker does not know, and then the boot's own validateRulesFile
// (main.go), so that what this agrees to is exactly what the boot agrees to. A pull request
// that changes the deployment's airlock.json runs it, so the file a reviewer is asked to read
// is one the broker has already agreed to serve.
//
// On a file it would serve it exits 0 and writes the report on stdout: what the file adds and
// removes against the file --against names, or against an empty rules file when that flag is
// absent, which is how a whole file reads as added. The report names each secret added and
// removed with its hosts, and for a secret both files hold, each host and header added and
// removed and a changed allow, kind or judge. Those last three are in the report beside the
// hosts and headers because they decide where a credential may go and what a judge may approve:
// allow is the methods and path prefixes it may travel on, kind the sort of credential the entry
// holds ("", "aws" or "azure"), and judge the longest lifetime a judge may give it. What is left
// for the reviewer is whether each host is the right one for its secret.
//
// On a file it would not serve it exits 1 with one line on stderr naming the file and what the
// refusal says: the entry for a validation refusal or for a name that is not a name a session
// can ask for, the parse position for a file that is not JSON. A usage error is exit 2 and the
// usage line. It reads no configuration, opens no database and binds nothing: the file and the
// --against file are all it reads.
//
// The --against file is read and parsed but never validated: the rules in force may be a file
// this broker would refuse, and what changed is the point of the report.
func airlockCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("broker "+airlockCheckCommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, airlockCheckUsage) }
	against := flags.String("against", "", "the rules file in force now, the one the report is made against")
	// The subcommand's form puts the file first and the flag after it, and flag.Parse stops at
	// the first argument that is not a flag, so the files are taken out one at a time and what
	// follows each is parsed again. A flag then works wherever it is written, and a second file
	// is still a usage error rather than an argument that is quietly ignored.
	var files []string
	rest := args
	for {
		if err := flags.Parse(rest); err != nil {
			// Parse has written what it refused and then the usage line.
			return 2
		}
		if flags.NArg() == 0 {
			break
		}
		files = append(files, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	if len(files) != 1 || strings.TrimSpace(files[0]) == "" {
		flags.Usage()
		return 2
	}
	path := strings.TrimSpace(files[0])
	rules, ok := readRulesFile(path, stderr)
	if !ok {
		return 1
	}
	if err := validateRulesFile(rules); err != nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", airlockCheckCommand, path, err)
		return 1
	}
	var old airlock.Rules
	oldName := "an empty rules file, so every entry reads as added"
	if name := strings.TrimSpace(*against); name != "" {
		if old, ok = readRulesFile(name, stderr); !ok {
			return 1
		}
		oldName = name
	}
	fmt.Fprintf(stdout, "%s: %s is valid: %s\n", airlockCheckCommand, path, secretCount(len(rules.Secrets)))
	fmt.Fprintf(stdout, "%s: compared against %s\n", airlockCheckCommand, oldName)
	writeAirlockChanges(stdout, old, rules)
	return 0
}

// readRulesFile reads and parses one rules file through the boot's own reader (main.go's
// parseRulesFile), and writes why it could not on stderr in this command's words: a path that
// cannot be read as os.ReadFile's own error, which names the file, and a file that is not an
// airlock rules file as airlock.Parse's error under the file's name, because that error carries
// the parse position and not the path. It validates nothing: the caller validates the new file
// alone, as readAirlockRules validates the file the boot is about to serve.
func readRulesFile(path string, stderr io.Writer) (airlock.Rules, bool) {
	rules, err := parseRulesFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", airlockCheckCommand, err)
		return airlock.Rules{}, false
	}
	return rules, true
}

// secretCount counts the entries of a file in words, as the end-sessions subcommand counts the
// sessions it ended.
func secretCount(n int) string {
	if n == 1 {
		return "1 secret"
	}
	return fmt.Sprintf("%d secrets", n)
}

// writeAirlockChanges writes the report: one line per fact, grouped by secret and in a stable
// order, secret names sorted and then hosts and headers sorted within each secret, so that two
// runs on the same pair of files write the same bytes and a reviewer can read two reports
// against each other. It writes one line saying so when the two files say the same thing.
//
// Where a file repeats a secret's name, the entry read is the first one, as airlock.Rules.Lookup
// and so the broker itself reads it. The new file is validated before this runs and a repeated
// name is a refusal there; only an old file can hold one.
//
// A secret only one file holds is reported against gone, an entry that binds no host and names no
// header, so that an added secret's headers, allow, kind and judge are in the report beside its
// hosts: a reviewer of a new entry needs to read where the value may go and in which header, not
// the host alone. gone names no header rather than leaving the field unset, because an unset
// headers field means the authorization default (airlock.SecretRule.HeaderNames), which an added
// entry has not removed.
func writeAirlockChanges(out io.Writer, old, now airlock.Rules) {
	gone := airlock.SecretRule{Headers: []string{}}
	changed := false
	for _, name := range secretNames(old, now) {
		was, is := old.Lookup(name), now.Lookup(name)
		switch {
		case is == nil:
			fmt.Fprintf(out, "secret %q: removed\n", name)
			writeEntryChanges(out, name, *was, gone)
			changed = true
		case was == nil:
			fmt.Fprintf(out, "secret %q: added\n", name)
			writeEntryChanges(out, name, gone, *is)
			changed = true
		default:
			if writeEntryChanges(out, name, *was, *is) {
				changed = true
			}
		}
	}
	if !changed {
		fmt.Fprintf(out, "%s: nothing changed\n", airlockCheckCommand)
	}
}

// writeEntryChanges writes what one secret both files hold has changed, and reports whether it
// wrote anything.
func writeEntryChanges(out io.Writer, name string, was, is airlock.SecretRule) bool {
	changed := writeListChanges(out, name, "host", was.Hosts, is.Hosts)
	if writeListChanges(out, name, "header", was.HeaderNames(), is.HeaderNames()) {
		changed = true
	}
	for _, field := range []struct{ what, was, is string }{
		{"allow", allowText(was.Allow), allowText(is.Allow)},
		{"kind", shown(was.Kind), shown(is.Kind)},
		{"judge", judgeText(was.Judge), judgeText(is.Judge)},
	} {
		if field.was != field.is {
			fmt.Fprintf(out, "secret %q: %s %s → %s\n", name, field.what, field.was, field.is)
			changed = true
		}
	}
	return changed
}

// writeListChanges writes one line for every value in exactly one of the two lists, sorted, and
// reports whether it wrote anything. The values are compared as the file spells them: the airlock
// matches a host and a header name case-insensitively, but a change of case is a change to the
// file, and the report's job is to say what the file changed.
func writeListChanges(out io.Writer, secret, what string, was, is []string) bool {
	changed := false
	for _, value := range sortedUnion(was, is) {
		switch {
		case !slices.Contains(was, value):
			fmt.Fprintf(out, "secret %q: %s %s added\n", secret, what, value)
			changed = true
		case !slices.Contains(is, value):
			fmt.Fprintf(out, "secret %q: %s %s removed\n", secret, what, value)
			changed = true
		}
	}
	return changed
}

// allowText reads one entry's allow list as a line of the report, with the two lists that mean
// something other than what they list in words (airlock.allowOK: a missing list allows any
// method and path, an empty one allows nothing).
func allowText(allow []airlock.Allow) string {
	switch {
	case allow == nil:
		return "any method and path"
	case len(allow) == 0:
		return "nothing"
	}
	rows := make([]string, len(allow))
	for i, a := range allow {
		rows[i] = shown(a.Method) + " " + shown(a.PathPrefix)
	}
	return strings.Join(rows, ", ")
}

// judgeText reads an entry's judge lifetime as a line of the report. No judge entry at all and a
// judge of zero seconds are different answers, so the empty one is a word and not a blank.
func judgeText(judge *uint32) string {
	if judge == nil {
		return "none"
	}
	return fmt.Sprintf("%d seconds", *judge)
}

// shown writes an empty field as a pair of quotes, so that a line of the report never ends in a
// blank where a reader cannot tell an empty value from a missing word.
func shown(value string) string {
	if value == "" {
		return `""`
	}
	return value
}

// secretNames is every name either file holds, sorted: the two files' names as sortedUnion,
// which is the same union-and-sort every other pair of lists in this report takes.
func secretNames(old, now airlock.Rules) []string {
	was := make([]string, 0, len(old.Secrets))
	for _, s := range old.Secrets {
		was = append(was, s.Name)
	}
	is := make([]string, 0, len(now.Secrets))
	for _, s := range now.Secrets {
		is = append(is, s.Name)
	}
	return sortedUnion(was, is)
}

// sortedUnion is every value in either list, once each, sorted.
func sortedUnion(was, is []string) []string {
	values := make(map[string]struct{}, len(was)+len(is))
	for _, v := range was {
		values[v] = struct{}{}
	}
	for _, v := range is {
		values[v] = struct{}{}
	}
	return slices.Sorted(maps.Keys(values))
}
