package ghtool

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// allowedCommands are the tool's commands by first word, each with the second words it allows:
// nil for every one but those deniedSubcommands names. Cobra's `ls` alias of list is listed where
// list is. Everything else is refused before anything runs: extension, alias, config, auth,
// codespace, browse, gist, secret, ssh-key, gpg-key, every repo command but view, every release
// command but view and list, every issue command but view, list and comment, and the rest.
var allowedCommands = map[string]map[string]bool{
	"api":      nil,
	"pr":       nil,
	"run":      nil,
	"workflow": nil,
	"search":   nil,
	"repo":     {"view": true},
	"release":  {"view": true, "list": true, "ls": true},
	"issue":    {"view": true, "list": true, "ls": true, "comment": true},
	"label":    {"list": true, "ls": true},
	"cache":    {"list": true, "ls": true},
}

// deniedSubcommands are the subcommands of an allowed group the tool refuses: `pr checkout`
// (`pr co`), which runs git into the working directory, and `run download`, which writes
// artifacts to the daemon's disk.
var deniedSubcommands = map[string]map[string]bool{
	"pr":  {"checkout": true, "co": true},
	"run": {"download": true},
}

// allowedList is the allow list as a refusal names it.
const allowedList = "api, pr (not pr checkout), run (not run download), workflow, repo view, release view|list, search, issue view|list|comment, label list, cache list"

// commandWords are the first two words of argv that name its command, as gh reads them: past the
// -R/--repo and --help gh lets come before the command's name. It stops at any other option, which
// it answers as a word, so the allow list refuses it by name.
func commandWords(argv []string) []string {
	var words []string
	for i := 0; i < len(argv) && len(words) < 2; i++ {
		arg := argv[i]
		switch {
		case arg == "-R" || arg == "--repo":
			i++
		case strings.HasPrefix(arg, "--repo=") || (strings.HasPrefix(arg, "-R") && len(arg) > 2) ||
			arg == "--help" || strings.HasPrefix(arg, "--help=") || arg == "-h":
		case strings.HasPrefix(arg, "-"):
			if len(words) == 0 {
				return []string{arg}
			}
			return words
		default:
			words = append(words, arg)
		}
	}
	return words
}

// allowed refuses a command whose words are none of the tool's, naming the refused word and the
// allowed set.
func allowed(words []string) string {
	if len(words) == 0 {
		return "a call names no gh command; the gh tool runs only " + allowedList
	}
	subcommands, ok := allowedCommands[words[0]]
	refused := ""
	switch {
	case !ok:
		refused = words[0]
	case len(words) > 1 && deniedSubcommands[words[0]][words[1]]:
		refused = words[1]
	case subcommands != nil && (len(words) < 2 || !subcommands[words[1]]):
		refused = strings.Join(words, " ")
	}
	if refused == "" {
		return ""
	}
	return fmt.Sprintf("`gh %s` is not one of the gh tool's commands (%q is refused); the gh tool runs only %s", strings.Join(words, " "), refused, allowedList)
}

// refusal is the reason argv, whose command is words, is refused before anything runs, "" for
// none: a command the allow list does not hold, an option opening a browser (--web or -w,
// anywhere), `gh api --hostname`, which reaches another host, and an option reading a file or
// standard input on the daemon's host (--body-file, a pr or issue command's -F, pr create's
// --recover, a typed field's @<path> for workflow run) — gh api's own options are read with its
// footer (withFooter).
func refusal(argv, words []string) string {
	if why := allowed(words); why != "" {
		return why
	}
	for i, arg := range argv {
		switch {
		case arg == "--web" || strings.HasPrefix(arg, "--web=") || strings.ContainsRune(shortRun(arg), 'w'):
			return fmt.Sprintf("%q opens a web browser (-w, --web), which the gh tool never does; an option that is not --web is spelled by its long name", arg)
		case arg == "--body-file" || strings.HasPrefix(arg, "--body-file="):
			return fmt.Sprintf("%q reads the text from a file, to which the gh tool cannot add the agent's footer; pass the text itself with --body", arg)
		case (words[0] == "pr" || words[0] == "issue") && strings.ContainsRune(shortRun(arg), 'F'):
			return fmt.Sprintf("%q reads the text from a file (-F, --body-file), to which the gh tool cannot add the agent's footer; pass the text itself with --body (an option that is not -F is spelled by its long name)", arg)
		case arg == "--recover" || strings.HasPrefix(arg, "--recover="):
			return fmt.Sprintf("%q reads a file on the daemon's host; pass the title and body with --title and --body", arg)
		case words[0] == "workflow":
			if value, ok := typedField(argv, i); ok && fieldReadsFile(value) {
				return fmt.Sprintf("%q reads a field's value from a file or standard input; pass the value itself with -f", value)
			}
		}
	}
	return ""
}

// shortRun is the letters of a single-dash option word up to its first other character: the
// shorthands it may stand for, -cw for -c and -w, or a shorthand and the start of its value, -Ffile.
// A word that is no single-dash option has none, and neither does -R, whose value the rest of the
// word is (-Rowner/repo).
func shortRun(arg string) string {
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' || arg[1] == 'R' {
		return ""
	}
	end := 1
	for end < len(arg) && (arg[end] >= 'a' && arg[end] <= 'z' || arg[end] >= 'A' && arg[end] <= 'Z') {
		end++
	}
	return arg[1:end]
}

// typedField is the key=value a typed field option (-F, --field) at argv[i] gives, and whether
// argv[i] is one.
func typedField(argv []string, i int) (string, bool) {
	arg := argv[i]
	switch {
	case (arg == "-F" || arg == "--field") && i+1 < len(argv):
		return argv[i+1], true
	case strings.HasPrefix(arg, "--field="):
		return strings.TrimPrefix(arg, "--field="), true
	case strings.HasPrefix(arg, "-F") && len(arg) > 2:
		return strings.TrimPrefix(strings.TrimPrefix(arg, "-F"), "="), true
	}
	return "", false
}

// fieldReadsFile says whether a typed field's key=value reads its value from a file or standard
// input, as gh reads `@<path>` and `@-`.
func fieldReadsFile(field string) bool {
	_, value, _ := strings.Cut(field, "=")
	return strings.HasPrefix(value, "@")
}

// canonicalRefusal refuses a command gh's own help names otherwise than its words do, when that
// changes what the tool allows or adds a footer to: a cobra alias of a refused command (`pr co`
// for pr checkout), or of a command whose footer the tool adds by its words.
func canonicalRefusal(words, command []string) string {
	if command == nil {
		return ""
	}
	if why := allowed(command[:min(2, len(command))]); why != "" {
		return why
	}
	if spoken, read := writeOf(words), writeOf(command); spoken != read {
		return fmt.Sprintf("`gh %s` runs `gh %s`; call it by that name", strings.Join(words, " "), strings.Join(command, " "))
	}
	return ""
}

// footer is the Legion footer of a comment or review the agent posts in answer to question with
// call seq: the agent's session and phase, which the daemon's review intake reads
// (intake.PullRequestReview.LegionSession), and the answer key, <question>.<seq>, by which a
// restart finds it (Lookup). json.Marshal escapes `>`, so nothing in it ends the comment early.
func footer(agent Agent, question string, seq int) string {
	encoded, err := json.Marshal(struct {
		Session string `json:"session"`
		Phase   string `json:"phase"`
		Answer  string `json:"answer"`
	}{agent.Session, agent.Phase, question + "." + strconv.Itoa(seq)})
	if err != nil {
		panic(err) // three strings always encode
	}
	return footerMarker + " " + string(encoded) + " -->"
}

// footerMarker opens a Legion footer (intake's legionFooterMarker).
const footerMarker = "<!-- legion:"

// flagSpec is one option of a command the tool reads for its footer: its shorthand (0 for none)
// and whether it takes a value.
type flagSpec struct {
	short byte
	value bool
}

// commandSpec is a command's options by long name, from gh 2.98.0's help.
type commandSpec map[string]flagSpec

var (
	helpFlag    = flagSpec{short: 'h'}
	repoFlag    = flagSpec{short: 'R', value: true}
	commentSpec = commandSpec{
		"body": {'b', true}, "body-file": {'F', true}, "create-if-none": {}, "delete-last": {}, "edit-last": {},
		"editor": {short: 'e'}, "web": {short: 'w'}, "yes": {}, "repo": repoFlag, "help": helpFlag,
	}
	reviewSpec = commandSpec{
		"approve": {short: 'a'}, "body": {'b', true}, "body-file": {'F', true}, "comment": {short: 'c'},
		"request-changes": {short: 'r'}, "repo": repoFlag, "help": helpFlag,
	}
	closeSpec  = commandSpec{"comment": {'c', true}, "delete-branch": {short: 'd'}, "repo": repoFlag, "help": helpFlag}
	reopenSpec = commandSpec{"comment": {'c', true}, "repo": repoFlag, "help": helpFlag}
	apiSpec    = commandSpec{
		"allow-escape-sequences": {}, "cache": {value: true}, "field": {'F', true}, "header": {'H', true},
		"hostname": {value: true}, "include": {short: 'i'}, "input": {value: true}, "jq": {'q', true},
		"method": {'X', true}, "paginate": {}, "preview": {'p', true}, "raw-field": {'f', true}, "silent": {},
		"slurp": {}, "template": {'t', true}, "verbose": {}, "help": helpFlag,
	}
)

// writes are the commands that post a comment or a review, by their words, and their options.
var writes = map[string]commandSpec{
	"pr comment":    commentSpec,
	"issue comment": commentSpec,
	"pr review":     reviewSpec,
	"pr close":      closeSpec,
	"pr reopen":     reopenSpec,
	"api":           apiSpec,
}

// writeOf is the key of writes a command's words name, "" for none.
func writeOf(words []string) string {
	if len(words) > 0 && words[0] == "api" {
		return "api"
	}
	if len(words) > 1 {
		if _, ok := writes[words[0]+" "+words[1]]; ok {
			return words[0] + " " + words[1]
		}
	}
	return ""
}

// occurrence is one option given: its long name, and for one taking a value the value and the
// index of the argument whose suffix the value is (the next argument, or the rest of the option's
// own word); for a boolean, its value and index -1.
type occurrence struct {
	name  string
	value string
	index int
}

// parsedArgs is a command's arguments as pflag reads them against its spec.
type parsedArgs struct {
	options     []occurrence
	positionals []string
}

// given are the values of the options named name, in order.
func (p parsedArgs) given(name string) []occurrence {
	var given []occurrence
	for _, o := range p.options {
		if o.name == name {
			given = append(given, o)
		}
	}
	return given
}

// set says whether the boolean option name was given true, its last value deciding.
func (p parsedArgs) set(name string) bool {
	given := p.given(name)
	if len(given) == 0 {
		return false
	}
	on, err := strconv.ParseBool(given[len(given)-1].value)
	return err == nil && on
}

// parseArgs reads argv, a whole command's arguments, as pflag reads them against spec: long
// options with = or the next argument, shorthand runs, `--` ending the options. An option spec
// does not hold is refused, since the tool cannot then be certain where the text it adds the
// footer to is.
func parseArgs(argv []string, spec commandSpec, command string) (parsedArgs, error) {
	shorts := map[byte]string{}
	for name, flag := range spec {
		if flag.short != 0 {
			shorts[flag.short] = name
		}
	}
	unknown := func(arg string) error {
		return fmt.Errorf("%q is no option of `gh %s` the gh tool reads; it reads the command's options to add the agent's footer to what it posts", arg, command)
	}
	var parsed parsedArgs
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			parsed.positionals = append(parsed.positionals, argv[i+1:]...)
			return parsed, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			flag, ok := spec[name]
			if !ok {
				return parsedArgs{}, unknown(arg)
			}
			switch {
			case !flag.value:
				if !hasValue {
					value = "true"
				}
				parsed.options = append(parsed.options, occurrence{name: name, value: value, index: -1})
			case hasValue:
				parsed.options = append(parsed.options, occurrence{name: name, value: value, index: i})
			case i+1 < len(argv):
				i++
				parsed.options = append(parsed.options, occurrence{name: name, value: argv[i], index: i})
			default:
				return parsedArgs{}, fmt.Errorf("%q has no value", arg)
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			for rest := arg[1:]; rest != ""; {
				name, ok := shorts[rest[0]]
				if !ok {
					return parsedArgs{}, unknown(arg)
				}
				rest = rest[1:]
				switch {
				case !spec[name].value:
					parsed.options = append(parsed.options, occurrence{name: name, value: "true", index: -1})
				case len(rest) > 1 && rest[0] == '=':
					parsed.options = append(parsed.options, occurrence{name: name, value: rest[1:], index: i})
					rest = ""
				case rest != "":
					parsed.options = append(parsed.options, occurrence{name: name, value: rest, index: i})
					rest = ""
				case i+1 < len(argv):
					i++
					parsed.options = append(parsed.options, occurrence{name: name, value: argv[i], index: i})
				default:
					return parsedArgs{}, fmt.Errorf("%q has no value", arg)
				}
			}
		default:
			parsed.positionals = append(parsed.positionals, arg)
		}
	}
	return parsed, nil
}

// withFooter is argv with the agent's footer added to every comment or review it posts, or the
// reason it is refused: a comment command's --body (pr comment, issue comment) and a closing or
// reopening --comment get the footer after their text; a review's --body too, and a bare
// --approve, --request-changes or --comment gets --body with the footer alone; a gh api write (any
// method but GET and HEAD) gets it after every literal body field (-f body=…, -F body=…), and a
// POST creating or submitting a review, whose body is optional, gets `-f body=<footer>` when it
// has none. A form whose text the tool cannot read is refused: --editor, gh api's --input and a
// typed field's @<path>, and a GraphQL mutation, which has a REST endpoint to use instead; so is
// gh api's --hostname (--body-file and a pr or issue command's -F are refused before, by
// refusal). Every other command, and a write that posts no comment or review (pr merge, run
// rerun, workflow run), runs as it is.
func withFooter(argv, words []string, foot string) ([]string, string) {
	key := writeOf(words)
	spec, ok := writes[key]
	if !ok {
		return argv, ""
	}
	parsed, err := parseArgs(argv, spec, key)
	if err != nil {
		return nil, err.Error()
	}
	run := slices.Clone(argv)
	appendTo := func(name string) bool {
		given := parsed.given(name)
		for _, o := range given {
			run[o.index] += "\n\n" + foot
		}
		return len(given) > 0
	}
	switch key {
	case "pr comment", "issue comment", "pr review":
		if parsed.set("editor") {
			return nil, "--editor opens an editor, which the gh tool has none of; pass the text itself with --body"
		}
		if !appendTo("body") && key == "pr review" && (parsed.set("approve") || parsed.set("request-changes") || parsed.set("comment")) {
			run = append(run, "--body", foot)
		}
	case "pr close", "pr reopen":
		appendTo("comment")
	case "api":
		return apiWithFooter(run, parsed, foot)
	}
	return run, ""
}

// apiReviewCreate is a gh api endpoint that creates or submits a pull request review, whose body
// is optional: a POST to it posts a review with no text unless it is given one.
var apiReviewCreate = regexp.MustCompile(`^repos/[^/]+/[^/]+/pulls/[^/]+/reviews(/[^/]+/events)?$`)

// graphQLMutation finds a mutation operation in a GraphQL document.
var graphQLMutation = regexp.MustCompile(`\bmutation\b`)

// apiWithFooter is withFooter's gh api: run the call's arguments, parsed as gh api reads them.
func apiWithFooter(run []string, parsed parsedArgs, foot string) ([]string, string) {
	if len(parsed.given("hostname")) > 0 {
		return nil, "--hostname sends the request to another host than github.com, which the gh tool never reaches"
	}
	if len(parsed.given("input")) > 0 {
		return nil, "--input reads the request's body from a file or standard input, to which the gh tool cannot add the agent's footer; pass each field with -f"
	}
	fields := append(parsed.given("raw-field"), parsed.given("field")...)
	for _, field := range parsed.given("field") {
		if fieldReadsFile(field.value) {
			return nil, fmt.Sprintf("%q reads a field's value from a file or standard input; pass the value itself with -f", field.value)
		}
	}
	endpoint := ""
	if len(parsed.positionals) > 1 {
		endpoint = apiPath(parsed.positionals[1])
	}
	if endpoint == "graphql" {
		for _, field := range fields {
			if name, value, _ := strings.Cut(field.value, "="); name == "query" && graphQLMutation.MatchString(value) {
				return nil, "a GraphQL mutation cannot carry the agent's footer; make the write through its REST endpoint (a comment: repos/<owner>/<repo>/issues/<number>/comments with -f body=<text>)"
			}
		}
	}
	method := "GET"
	if len(fields) > 0 {
		method = "POST"
	}
	if given := parsed.given("method"); len(given) > 0 {
		method = strings.ToUpper(given[len(given)-1].value)
	}
	if method == "GET" || method == "HEAD" {
		return run, ""
	}
	bodies := 0
	for _, field := range fields {
		if name, _, _ := strings.Cut(field.value, "="); name == "body" {
			run[field.index] += "\n\n" + foot
			bodies++
		}
	}
	if bodies == 0 && method == "POST" && apiReviewCreate.MatchString(endpoint) {
		run = append(run, "-f", "body="+foot)
	}
	return run, ""
}

// apiPath is a gh api endpoint as a path, for matching: no API host, leading slash or query.
func apiPath(endpoint string) string {
	path := endpoint
	if lower := strings.ToLower(path); strings.HasPrefix(lower, "https://api.github.com/") {
		path = path[len("https://api.github.com/"):]
	}
	path, _, _ = strings.Cut(strings.TrimPrefix(path, "/"), "?")
	return path
}
