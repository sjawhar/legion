// Package sampler is Legion's resource sampler (INFRA-1208): the CPU and memory a pod's processes
// spend, read through internal/procread and served as Prometheus series — in an issue pod split by
// role and by the kind of work each process does, on a node split by pod.
//
// This file is the kind rules, the one place that decides from a process's program and arguments
// alone whether it is build work, test work or a tool. It reads a command line and answers a Kind:
// nothing it reads leaves this package, which is how dispatch://INFRA-1208/spec#b-a-noargs — no
// series carries a command line, an argument or an environment value — stays true of a sampler
// that must read command lines to do its job at all.
package sampler

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
)

// Kind is the work a process's CPU and memory are counted as, and the value of the sampler's
// `kind` label. A series carries one of these words and nothing else about what ran.
type Kind string

const (
	// KindAgent is a role's own Oh My Pi process, the program the role runs, and Legion's own
	// processes for that role. No rule answers it: a process is an agent by who started it,
	// which the walk knows and a command line does not.
	KindAgent Kind = "agent"
	// KindBuild is a compiler, a linker or a build driver: what turns sources into artefacts.
	KindBuild Kind = "build"
	// KindTest is a test runner, and the builds it does on the way are its own.
	KindTest Kind = "test"
	// KindTool is everything else an agent runs: a language server, a formatter, a shell, the
	// walk's answer for a child of an agent that no rule and no ancestor names.
	KindTool Kind = "tool"
	// KindOther is node mode's word for work that is neither build nor test. A node's sampler
	// knows nothing of roles and splits a pod's processes three ways
	// (dispatch://INFRA-1208/spec#b-a-node), where an issue pod's splits them by role.
	KindOther Kind = "other"
	// KindShared is the launcher, the sampler itself and the test Postgres: a process in no
	// role's subtree, which the sampler reports under the role `shared` and the kind `shared`
	// (dispatch://INFRA-1208/spec#b-a-sampler). It is a kind on the wire and never a rule's
	// answer, since what makes a process shared is whose subtree it is outside of.
	KindShared Kind = "shared"
)

// ruleKinds are the kinds a rule may answer: the ones a program and its arguments can tell on
// their own. KindAgent and KindShared are not among them, because neither is a property of a
// command line — a process is an agent's or shared by where it sits in the process tree, which
// the walk reads and a rule cannot.
var ruleKinds = []Kind{KindBuild, KindTest, KindTool, KindOther}

// patternCharacters are the characters a glob or a regular expression is written with. A rule is
// matched literally, whole word for whole word, so a pattern holding one of these could match
// nothing at all, and an operator who wrote `go*` or `(gcc|clang)` meant something this engine
// does not do: refusing the rule says so, where accepting it would silently count nothing. `+`
// and `.` are not among them, since real programs are named `c++` and `clang++` and a literal
// match makes them harmless.
const patternCharacters = `*?[]{}()|^$\`

// Rule is one rule as an operator writes it: a program, the run of arguments that must follow it
// where the rule names one, and the kind the match answers. It is the shape of an entry of the
// daemon configuration's `runtime.kubernetes.sampler_rules` and of the JSON the daemon hands the
// sampler on a flag, which is why its fields are exported and tagged.
//
// A pattern is one whole word, matched for equality and never as a substring, a glob or a regular
// expression: `gopls` matches a process whose program is `gopls` and not one named
// `my-gopls-wrapper`, and the arguments of `{"program":"go","arguments":["test"]}` match the word
// `test` immediately after the word `go`. A rule names a program by its last path element alone,
// since that is what the engine compares against. NewRules refuses anything else.
type Rule struct {
	Program   string   `json:"program"`
	Arguments []string `json:"arguments,omitempty"`
	Kind      Kind     `json:"kind"`
}

// String is the rule's pattern, the program and then its arguments, which is how an error names
// the rule it refuses. It renders the operator's own configuration and never a process's command
// line: no rule holds anything read from a process.
func (r Rule) String() string {
	if len(r.Arguments) == 0 {
		return r.Program
	}
	return r.Program + " " + strings.Join(r.Arguments, " ")
}

// matches reports whether this rule's program stands among these words with its arguments, if it
// names any, as the words immediately after it. Every word is compared by its last path element,
// so `/usr/bin/go test` is the `go test` rule as plainly as `go test` is.
func (r Rule) matches(words []string) bool {
	for i, word := range words {
		if path.Base(word) != r.Program {
			continue
		}
		rest := words[i+1:]
		if len(rest) < len(r.Arguments) {
			// Every later position leaves fewer words still.
			return false
		}
		if slices.Equal(rest[:len(r.Arguments)], r.Arguments) {
			return true
		}
	}
	return false
}

// validate refuses a rule this engine could not match as its author meant it, naming the rule.
func (r Rule) validate() error {
	if r.Program == "" {
		return errors.New("sampler: a rule with no program")
	}
	if strings.Contains(r.Program, "/") {
		return fmt.Errorf("sampler: rule %q names a program by its path: name its last path element alone, %q", r, path.Base(r.Program))
	}
	if err := r.validateWord(r.Program); err != nil {
		return err
	}
	for _, argument := range r.Arguments {
		if argument == "" {
			return fmt.Errorf("sampler: rule %q has an empty argument", r)
		}
		if err := r.validateWord(argument); err != nil {
			return err
		}
	}
	if !slices.Contains(ruleKinds, r.Kind) {
		return fmt.Errorf("sampler: rule %q has kind %q: a rule answers one of %s", r, r.Kind, namedKinds(ruleKinds))
	}
	return nil
}

// validateWord refuses a program or an argument that is not one whole word matched literally.
func (r Rule) validateWord(word string) error {
	if strings.ContainsFunc(word, unicode.IsSpace) {
		return fmt.Errorf("sampler: rule %q holds %q, which is more than one word: name the program and then each argument of the run on its own", r, word)
	}
	if strings.ContainsAny(word, patternCharacters) {
		return fmt.Errorf("sampler: rule %q holds %q: a rule matches a whole word literally, never a glob or a regular expression", r, word)
	}
	return nil
}

// namedKinds is a list of kinds as an error names them.
func namedKinds(kinds []Kind) string {
	names := make([]string, len(kinds))
	for i, kind := range kinds {
		names[i] = string(kind)
	}
	return strings.Join(names, ", ")
}

// builtinRules are the toolchains every deployment knows, in the order they are tried.
//
// The order is the rule and not a heuristic, because a command line is read once and the process
// it names keeps that kind for its whole life: `go test ./... || go build ./...` is one shell, and
// whichever kind the first matching rule gives it is the kind every second of its CPU is counted
// as. So the order is fixed here and written down rather than guessed at per line. Test comes
// before build because a test run builds what it tests — the build in such a line is the test's
// own — while a build that runs no test names no test runner. Tool comes before build for the
// same reason in reverse: `lean --server` is a language server all day and `lean Foo.lean` is a
// compile, and only the more specific rule, the one that names an argument, tells them apart, so
// it must be reached first.
var builtinRules = []Rule{
	// test
	{Program: "go", Arguments: []string{"test"}, Kind: KindTest},
	{Program: "cargo", Arguments: []string{"test"}, Kind: KindTest},
	{Program: "bun", Arguments: []string{"test"}, Kind: KindTest},
	{Program: "node", Arguments: []string{"--test"}, Kind: KindTest},
	{Program: "vitest", Kind: KindTest},
	{Program: "jest", Kind: KindTest},
	{Program: "pytest", Kind: KindTest},
	{Program: "python", Arguments: []string{"-m", "pytest"}, Kind: KindTest},
	{Program: "python", Arguments: []string{"-m", "unittest"}, Kind: KindTest},

	// tool
	{Program: "gopls", Kind: KindTool},
	{Program: "lean", Arguments: []string{"--server"}, Kind: KindTool},
	{Program: "rust-analyzer", Kind: KindTool},

	// build
	{Program: "go", Arguments: []string{"build"}, Kind: KindBuild},
	{Program: "go", Arguments: []string{"install"}, Kind: KindBuild},
	{Program: "go", Arguments: []string{"vet"}, Kind: KindBuild},
	{Program: "cc", Kind: KindBuild},
	{Program: "gcc", Kind: KindBuild},
	{Program: "clang", Kind: KindBuild},
	{Program: "c++", Kind: KindBuild},
	{Program: "g++", Kind: KindBuild},
	{Program: "clang++", Kind: KindBuild},
	{Program: "ld", Kind: KindBuild},
	{Program: "lld", Kind: KindBuild},
	{Program: "rustc", Kind: KindBuild},
	{Program: "cargo", Arguments: []string{"build"}, Kind: KindBuild},
	{Program: "make", Kind: KindBuild},
	{Program: "ninja", Kind: KindBuild},
	{Program: "lake", Arguments: []string{"build"}, Kind: KindBuild},
	{Program: "lean", Kind: KindBuild},
	{Program: "coqc", Kind: KindBuild},
	{Program: "rocq", Arguments: []string{"compile"}, Kind: KindBuild},
}

// Rules is the kind engine: a deployment's own rules, then the built-in list. The zero Rules is
// the built-in list alone, which is what a deployment that configured none gets.
type Rules struct{ operator []Rule }

// NewRules is the engine of a deployment whose configuration holds these rules, after refusing
// every rule it could not match as written. The deployment's rules are tried before the built-in
// list, so one of them can override a built-in rule rather than having to wait behind it.
//
// It refuses a rule whose kind is not one a rule may answer, and two rules with the same program
// and the same arguments, naming the rule in each case: the second of two such rules can never be
// reached, and when their kinds differ the configuration says two things at once.
func NewRules(operator []Rule) (Rules, error) {
	seen := make(map[string]bool, len(operator))
	for _, rule := range operator {
		if err := rule.validate(); err != nil {
			return Rules{}, err
		}
		if seen[rule.String()] {
			return Rules{}, fmt.Errorf("sampler: rule %q twice", rule)
		}
		seen[rule.String()] = true
	}
	return Rules{operator: operator}, nil
}

// Kind is the kind of work the process with this argument vector does, and whether any rule named
// it at all. A program no rule names has no kind of its own and the answer is false: it is for the
// caller to say what that means, and the walk says the process takes the kind of the nearest
// ancestor that has one, or KindTool under an agent.
//
// The whole argument vector is matched and not the program alone, because the work a shell is told
// to do is the work it does: `bash -c "go build ./..."` spends its CPU on a build, and the word
// `bash` says nothing about that. An argument holding several words is matched as those words, so
// the shell's `-c` argument is read as the command it holds; every other argument is one word.
// Matching is whole word for whole word, never a substring of one.
//
// The answer is a Kind and nothing else — never the arguments it read, nor anything derived from
// them. dispatch://INFRA-1208/spec#b-a-noargs holds that no series carries a command line, an
// argument or an environment value, and this signature is how that is kept: the one place that
// reads a command line can hand its reader nothing to put in a label.
func (r Rules) Kind(args []string) (Kind, bool) {
	if len(args) == 0 {
		return "", false
	}
	words := commandWords(args)
	for _, rule := range r.operator {
		if rule.matches(words) {
			return rule.Kind, true
		}
	}
	for _, rule := range builtinRules {
		if rule.matches(words) {
			return rule.Kind, true
		}
	}
	return "", false
}

// commandWords is an argument vector as the words a rule is matched against: an argument that
// holds several words split into them, every other argument as it stands. A shell's command is one
// argument of the shell, so splitting is what lets the command inside it be read; and because
// almost every argument of almost every process is one word, a vector that needs no splitting is
// passed through and costs nothing.
func commandWords(args []string) []string {
	split := false
	for _, arg := range args {
		if strings.ContainsFunc(arg, unicode.IsSpace) {
			split = true
			break
		}
	}
	if !split {
		return args
	}
	words := make([]string, 0, len(args))
	for _, arg := range args {
		words = append(words, strings.Fields(arg)...)
	}
	return words
}
