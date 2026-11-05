package sampler

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The built-in list decides the kind of a command line, and these are the lines a Legion pod
// runs. A case with no kind is a program the list does not name: the answer is false, and what
// that process is counted as is the walk's to say, never this engine's.
func TestKindOfACommandLine(t *testing.T) {
	for _, test := range []struct {
		args []string
		want Kind
	}{
		// A shell is matched by what it was told to do, not by being a shell.
		{[]string{"bash", "-c", "go build ./..."}, KindBuild},
		{[]string{"bash", "-c", "cd packages/daemon && make all"}, KindBuild},
		{[]string{"sh", "-c", "go vet ./..."}, KindBuild},
		// Test before build: one line holding both words is a test run, which builds
		// what it tests.
		{[]string{"bash", "-c", "go test ./... || go build ./..."}, KindTest},
		{[]string{"bash", "-c", "go build ./... && go test ./..."}, KindTest},
		// A language server is a tool, and the same program compiling is a build. Only
		// the argument tells them apart, so the rule that names one is reached first.
		{[]string{"gopls"}, KindTool},
		{[]string{"gopls", "-remote=auto"}, KindTool},
		{[]string{"rust-analyzer"}, KindTool},
		{[]string{"lean", "--server"}, KindTool},
		{[]string{"lean", "Foo.lean"}, KindBuild},
		// The program is the last path element of the first argument.
		{[]string{"/usr/bin/go", "test", "./..."}, KindTest},
		{[]string{"/usr/lib/ccache/gcc", "-c", "a.c"}, KindBuild},
		{[]string{"bash", "-c", "/usr/bin/go build ./..."}, KindBuild},
		// Python's test runners, and a Python that runs no test.
		{[]string{"python", "-m", "pytest"}, KindTest},
		{[]string{"python", "-m", "unittest", "discover"}, KindTest},
		{[]string{"pytest", "-q"}, KindTest},
		{[]string{"python", "script.py"}, ""},
		// The rest of the built-in list.
		{[]string{"cargo", "test"}, KindTest},
		{[]string{"cargo", "build", "--release"}, KindBuild},
		{[]string{"bun", "test"}, KindTest},
		{[]string{"node", "--test"}, KindTest},
		{[]string{"node", "server.js"}, ""},
		{[]string{"vitest", "run"}, KindTest},
		{[]string{"jest"}, KindTest},
		{[]string{"go", "install", "./cmd/legion"}, KindBuild},
		{[]string{"cc", "-O2", "-c", "a.c"}, KindBuild},
		{[]string{"clang", "-c", "a.c"}, KindBuild},
		{[]string{"c++", "-std=c++20", "-c", "a.cc"}, KindBuild},
		{[]string{"g++", "-c", "a.cc"}, KindBuild},
		{[]string{"clang++", "-c", "a.cc"}, KindBuild},
		{[]string{"ld", "-o", "a.out", "a.o"}, KindBuild},
		{[]string{"lld", "-o", "a.out", "a.o"}, KindBuild},
		{[]string{"rustc", "main.rs"}, KindBuild},
		{[]string{"ninja", "-j8"}, KindBuild},
		{[]string{"lake", "build"}, KindBuild},
		{[]string{"coqc", "Foo.v"}, KindBuild},
		{[]string{"rocq", "compile", "Foo.v"}, KindBuild},
		// A whole word, never a substring of one: a wrapper named after a tool is not
		// that tool, and a program whose name begins with another's is not that one.
		{[]string{"my-gopls-wrapper"}, ""},
		{[]string{"gotestsum", "--format", "short"}, ""},
		{[]string{"makefile-lint"}, ""},
		// A run of arguments is next to each other, and a bare program the list names
		// only with an argument is no kind at all.
		{[]string{"go", "--flag", "test"}, ""},
		{[]string{"go"}, ""},
		{[]string{"cargo"}, ""},
		// A kernel thread has no argument vector, and nothing is no kind.
		{nil, ""},
		{[]string{}, ""},
	} {
		var rules Rules
		kind, ok := rules.Kind(test.args)
		if ok != (test.want != "") {
			t.Errorf("Kind(%q) = %q, %t, want ok %t", test.args, kind, ok, test.want != "")
			continue
		}
		if kind != test.want {
			t.Errorf("Kind(%q) = %q, want %q", test.args, kind, test.want)
		}
	}
}

// The zero Rules is the built-in list: a deployment that configures no rule of its own gets the
// toolchains everybody runs, and no caller has to remember to ask for them.
func TestTheZeroRulesIsTheBuiltInList(t *testing.T) {
	var rules Rules
	if kind, ok := rules.Kind([]string{"gopls"}); !ok || kind != KindTool {
		t.Errorf("the zero Rules on %q = %q, %t, want %q, true", "gopls", kind, ok, KindTool)
	}
}

// A deployment's own rules are tried before the built-in list, so one of them overrides a built-in
// rule rather than waiting behind it, and a rule for a toolchain the list does not know adds one.
func TestADeploymentsRuleIsTriedBeforeTheBuiltInList(t *testing.T) {
	rules, err := NewRules([]Rule{
		{Program: "go", Arguments: []string{"build"}, Kind: KindTool},
		{Program: "zig", Arguments: []string{"build"}, Kind: KindBuild},
	})
	if err != nil {
		t.Fatalf("NewRules() = %v", err)
	}
	for _, test := range []struct {
		args []string
		want Kind
	}{
		{[]string{"go", "build", "./..."}, KindTool},
		{[]string{"bash", "-c", "go build ./..."}, KindTool},
		{[]string{"zig", "build"}, KindBuild},
		{[]string{"go", "vet", "./..."}, KindBuild},
		{[]string{"gopls"}, KindTool},
	} {
		kind, ok := rules.Kind(test.args)
		if !ok || kind != test.want {
			t.Errorf("Kind(%q) = %q, %t, want %q, true", test.args, kind, ok, test.want)
		}
	}
}

// A kind a rule cannot answer is refused when the rules are read, naming the rule and the word:
// the alternative is a deployment whose rule quietly labels nothing, found months later in a
// dashboard with a kind nobody queries. KindAgent and KindShared are refused too, because what
// makes a process an agent's or shared is where it sits in the process tree and not what it runs.
func TestNewRulesRefusesAKindARuleCannotAnswer(t *testing.T) {
	for _, test := range []struct {
		kind Kind
		name string
	}{
		{"compile", "compile"},
		{"", ""},
		{KindAgent, "agent"},
		{KindShared, "shared"},
		{"Build", "Build"},
	} {
		_, err := NewRules([]Rule{{Program: "zig", Arguments: []string{"build"}, Kind: test.kind}})
		if err == nil {
			t.Errorf("NewRules(a rule of kind %q) = no error, want one", test.kind)
			continue
		}
		for _, want := range []string{fmt.Sprintf("%q", test.name), `"zig build"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("NewRules(a rule of kind %q) = %v, want an error naming %s", test.kind, err, want)
			}
		}
	}
}

// Two rules with the same program and the same arguments are refused, naming the rule: the second
// can never be reached, and when the two answer different kinds the configuration says two things
// at once.
func TestNewRulesRefusesTwoRulesThatAreTheSame(t *testing.T) {
	for _, test := range []struct {
		name  string
		rules []Rule
	}{
		{"the same rule twice", []Rule{
			{Program: "zig", Arguments: []string{"build"}, Kind: KindBuild},
			{Program: "zig", Arguments: []string{"build"}, Kind: KindBuild},
		}},
		{"one match, two kinds", []Rule{
			{Program: "zig", Arguments: []string{"build"}, Kind: KindBuild},
			{Program: "zig", Arguments: []string{"build"}, Kind: KindTool},
		}},
	} {
		_, err := NewRules(test.rules)
		if err == nil {
			t.Errorf("NewRules(%s) = no error, want one", test.name)
			continue
		}
		if !strings.Contains(err.Error(), `"zig build"`) {
			t.Errorf("NewRules(%s) = %v, want an error naming %q", test.name, err, "zig build")
		}
	}
}

// A rule holds whole words and nothing else: no command line in one string, no path, no glob and
// no regular expression. Each of those would match nothing, so each is refused where it is
// written rather than counted as zero seconds for the life of the deployment.
func TestNewRulesRefusesAPatternThatIsNotWholeWords(t *testing.T) {
	for _, test := range []struct {
		name string
		rule Rule
		want string
	}{
		{"a command line as a program", Rule{Program: "go test", Kind: KindTest}, `"go test"`},
		{"a command line as an argument", Rule{Program: "go", Arguments: []string{"test ./..."}, Kind: KindTest}, `"test ./..."`},
		{"a program by its path", Rule{Program: "/usr/bin/go", Arguments: []string{"test"}, Kind: KindTest}, `"go"`},
		{"a glob", Rule{Program: "go*", Kind: KindBuild}, `"go*"`},
		{"an alternation", Rule{Program: "cc", Arguments: []string{"(-c|-S)"}, Kind: KindBuild}, `"(-c|-S)"`},
		{"an anchored expression", Rule{Program: "^gcc$", Kind: KindBuild}, `"^gcc$"`},
		{"no program at all", Rule{Arguments: []string{"test"}, Kind: KindTest}, "no program"},
		{"an empty argument", Rule{Program: "go", Arguments: []string{""}, Kind: KindBuild}, "empty argument"},
	} {
		_, err := NewRules([]Rule{test.rule})
		if err == nil {
			t.Errorf("NewRules(%s) = no error, want one", test.name)
			continue
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("NewRules(%s) = %v, want an error holding %s", test.name, err, test.want)
		}
	}
}

// The built-in list is held to the rules a deployment's own list is held to: every program and
// argument one whole word, every kind one a rule may answer, and no rule twice.
func TestTheBuiltInListIsItselfAValidRuleList(t *testing.T) {
	if _, err := NewRules(builtinRules); err != nil {
		t.Errorf("NewRules(builtinRules) = %v", err)
	}
}

// The engine answers a kind and nothing else. dispatch://INFRA-1208/spec#b-a-noargs holds that no
// series carries a command line, an argument or an environment value, and the way that is kept
// true in the one place that reads a command line is that its reader is handed nothing to put in
// a label: the answer is a string kind and a bool, the rule type's exported fields are the three
// an operator writes, and matching leaves the engine as it was.
func TestTheAnswerIsAKindAndNeverTheArgumentsItRead(t *testing.T) {
	method, ok := reflect.TypeOf(Rules{}).MethodByName("Kind")
	if !ok {
		t.Fatal("Rules has no Kind method")
	}
	answer := method.Type
	if answer.NumOut() != 2 {
		t.Fatalf("Rules.Kind answers %d values, want 2: a kind and whether a rule named it", answer.NumOut())
	}
	if answer.Out(0) != reflect.TypeOf(Kind("")) || answer.Out(0).Kind() != reflect.String {
		t.Errorf("Rules.Kind answers %s first, want a string Kind", answer.Out(0))
	}
	if answer.Out(1).Kind() != reflect.Bool {
		t.Errorf("Rules.Kind answers %s second, want a bool", answer.Out(1))
	}

	var exported []string
	rule := reflect.TypeOf(Rule{})
	for i := range rule.NumField() {
		if field := rule.Field(i); field.IsExported() {
			exported = append(exported, field.Name)
		}
	}
	if want := []string{"Program", "Arguments", "Kind"}; !slices.Equal(exported, want) {
		t.Errorf("Rule's exported fields are %q, want %q, each one an operator writes", exported, want)
	}
	engine := reflect.TypeOf(Rules{})
	for i := range engine.NumField() {
		if field := engine.Field(i); field.IsExported() {
			t.Errorf("Rules has the exported field %s, which a caller could read a command line out of", field.Name)
		}
	}

	const secret = "s3cret-token"
	rules, err := NewRules([]Rule{{Program: "zig", Arguments: []string{"build"}, Kind: KindBuild}})
	if err != nil {
		t.Fatalf("NewRules() = %v", err)
	}
	before := fmt.Sprintf("%#v", rules)
	if kind, ok := rules.Kind([]string{"bash", "-c", "go build -ldflags=-X=main.token=" + secret + " ./..."}); !ok || kind != KindBuild {
		t.Fatalf("Kind(a build carrying a secret) = %q, %t, want %q, true", kind, ok, KindBuild)
	}
	after := fmt.Sprintf("%#v", rules)
	if after != before {
		t.Errorf("matching changed the engine from %s to %s: it kept something of the line it read", before, after)
	}
	if strings.Contains(after, secret) {
		t.Errorf("the engine holds %q after matching a line that carried it", secret)
	}
}
