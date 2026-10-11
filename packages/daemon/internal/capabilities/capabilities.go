// Package capabilities is the one list of what a Legion worker can do (LEGION-578: every worker is
// a full agent). Each capability is checked at one site — in the image by `legion probe-image`
// (CheckImage), by each session at its start, which reports the row with its ready and the daemon
// renders (LEGION-663), by the daemon from the deployment's configuration, or withheld by a
// ruling — and the probe prints the whole table, one line per row, so an operator reads a single
// declared list in the probe pod's log rather than inferring what was checked from which probes
// happened to run.
package capabilities

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Name is a capability as the probe prints it and legion.yaml names it.
type Name string

const (
	Subagents            Name = "subagents"
	EvalJS               Name = "eval-js"
	EvalPython           Name = "eval-python"
	Browser              Name = "browser"
	LSP                  Name = "lsp"
	CodeGraph            Name = "codegraph"
	WebSearch            Name = "web-search"
	Skills               Name = "skills"
	MCP                  Name = "mcp"
	RepositoryExtensions Name = "repository-extensions"
	DispatchEnvoyTools   Name = "dispatch-envoy-tools"
	GitHub               Name = "github"
	Secrets              Name = "secrets"
	ModelFallback        Name = "model-fallback"
	Toolchain            Name = "toolchain"
	ResourceLimits       Name = "resource-limits"
	PoolCapacity         Name = "pool-capacity"
	Network              Name = "network"
	OperatorSetup        Name = "operator-setup"
	ProductionIdentities Name = "production-identities"
)

// Site is where a capability is checked.
type Site string

const (
	// SiteImage is checked inside the worker image by `legion probe-image` (CheckImage): the
	// image either carries what the capability needs or it does not.
	SiteImage Site = "image"
	// SiteLive is proved by each session at its start: the session measures the row and reports
	// it with its ready (claim.ReadyRequest.Capabilities), and the daemon renders the row from the
	// live sessions' reports (Deployment.Sessions; LEGION-663).
	SiteLive Site = "live"
	// SiteDeployment is decided by the deployment's legion.yaml, which the daemon reports: no
	// image or pod can show it.
	SiteDeployment Site = "deployment"
	// SiteWithheld is withheld by a ruling the row cites: a worker never gets it, by design.
	SiteWithheld Site = "withheld"
)

// Capability is one row of the table.
type Capability struct {
	Name Name
	Site Site
	// Summary is one line: what a worker does with it.
	Summary string
	// Ruling is the dispatch://LEGION-<n> a withheld row cites; for a live row, the issue whose
	// check proves it (never ""); "" otherwise.
	Ruling string
	// Awaits is, on an image row whose tooling the image carries but a pod's agent cannot use yet,
	// the sentence that says so and names the issue whose landing changes that; "" otherwise. The
	// report's job is to name what a worker lacks, so a tool the image carries but no pod loads is
	// not present: CheckImage and Deployment.Report render such a row installed, with this sentence
	// after the evidence, rather than present. No row awaits at present: CodeGraph did, until
	// LEGION-629 turned extension discovery on in the pod's launch (runtime/sandbox/manifest.go,
	// agentArgv), so the profile's plugin now loads in every pod.
	Awaits string
}

// Table is every capability, in the order the probe prints them: the declared list, printed
// whole. Image rows are checked by CheckImage; the others are rendered as what they are.
var Table = []Capability{
	{Subagents, SiteLive, "dispatches task subagents, each on the model its role configures", "dispatch://LEGION-663", ""},
	{EvalJS, SiteImage, "evaluates JavaScript in Oh My Pi's own runtime", "", ""},
	{EvalPython, SiteImage, "evaluates Python through the interpreter `omp setup python` manages", "", ""},
	{Browser, SiteImage, "drives a headless Chromium through the browser tools", "", ""},
	{LSP, SiteImage, "reads diagnostics and symbols from the Go, TypeScript and Python language servers", "", ""},
	{CodeGraph, SiteImage, "queries the CodeGraph index for affected tests, impact and callers", "", ""},
	{WebSearch, SiteLive, "searches the web through the web-search tool", "dispatch://LEGION-663", ""},
	{Skills, SiteImage, "loads the skills Legion's prompts name", "", ""},
	{MCP, SiteLive, "reaches the MCP servers the deployment configures", "dispatch://LEGION-663", ""},
	{RepositoryExtensions, SiteLive, "loads the Oh My Pi extensions the repository it works carries", "dispatch://LEGION-629", ""},
	{DispatchEnvoyTools, SiteLive, "reaches Dispatch through the `dispatch` command and Envoy through the pi-envoy tools", "dispatch://LEGION-663", ""},
	{GitHub, SiteLive, "reads and writes GitHub through its plain `gh` and `git` on its role's App token file", "dispatch://LEGION-631", ""},
	{Secrets, SiteDeployment, "reads the secrets the deployment grants its pod generation through the agent-secrets broker", "", ""},
	{ModelFallback, SiteDeployment, "falls back to another model when its own is unavailable (retry.modelFallback)", "", ""},
	{Toolchain, SiteImage, "builds and runs with the generic toolchain: go, curl, wget, python3, node, bun, uv and kubectl", "", ""},
	{ResourceLimits, SiteDeployment, "runs within the CPU and memory the deployment's pod resources give it", "", ""},
	{PoolCapacity, SiteDeployment, "starts in a Legion pool with room for the image probe's reservation at boot", "", ""},
	{Network, SiteWithheld, "reaches beyond the pod's own network: the pod is the boundary", "dispatch://LEGION-5", ""},
	{OperatorSetup, SiteWithheld, "installs or configures its own dependencies in the pod: Legion owns its dependencies", "dispatch://LEGION-200", ""},
	{ProductionIdentities, SiteWithheld, "acts under a production identity of its own", "dispatch://LEGION-551, dispatch://LEGION-205", ""},
}

// Decidable is the SiteDeployment names in Table order: what a legion.yaml decided line may name,
// and what the daemon measures from its deployment (Deployment.Report).
func Decidable() []Name { return namesAtSite(SiteDeployment) }

// Live is the SiteLive names in Table order: the rows each session measures at its start and
// reports with its ready, which Normalize fills out and Deployment.Report renders.
func Live() []Name { return namesAtSite(SiteLive) }

// namesAtSite is the names of Table's rows checked at site, in Table order.
func namesAtSite(site Site) []Name {
	var names []Name
	for _, row := range Table {
		if row.Site == site {
			names = append(names, row.Name)
		}
	}
	return names
}

// Row is one live row's measurement as a session reported it, normalised (Normalize). Its JSON is
// the daemon log's `rows` attribute on a report (one object per row, as the wire spells them); the
// store and the state each keep a shape of their own. A row built from what a session sent or what
// the store kept is built by NewRow.
type Row struct {
	Name Name `json:"name"`
	// OK is whether the session's check of the row passed.
	OK bool `json:"ok"`
	// Detail is the fact the check found, passed or not, cleaned (cleanDetail): it reaches the daemon
	// log, the store, the unauthenticated state route and the controller's daily report with nothing
	// delimiting it. A row that is not OK always has one — "no detail reported" when the session
	// sent none — since the state's `open[].detail` is a non-empty string; an OK row's may be empty,
	// a passing check with nothing to add.
	Detail string `json:"detail"`
}

// NewRow is the row a detail from outside the daemon makes, a session's ready or the store: the
// detail cleaned (cleanDetail), and "no detail reported" for a failing row left with none. Every
// such row is built here, so a detail reaches the log, the store and the state cleaned whichever
// way it came in, and a stored report a daemon kept before it cleaned something is cleaned when a
// later one reads it.
func NewRow(name Name, ok bool, detail string) Row {
	detail = cleanDetail(detail)
	if !ok && detail == "" {
		detail = "no detail reported"
	}
	return Row{Name: name, OK: ok, Detail: detail}
}

// Report is one session's normalised report of the live rows, as the daemon keeps and persists it:
// which claim and generation reported, from which process, when the session measured and when the
// daemon took the report, how long the measuring took, and the rows.
type Report struct {
	Claim      claim.Token
	Generation uint64
	// Locator is the process that reported; its Incarnation fences a stale report, since a claim
	// relaunched since the report is another process whose own ready reports anew.
	Locator runtime.Locator
	// MeasuredAt is when the session measured (claim.CapabilityReport.MeasuredAt); ReportedAt is
	// when the daemon took its ready.
	MeasuredAt time.Time
	ReportedAt time.Time
	// ElapsedMs is how long the session's measuring took.
	ElapsedMs int
	// Rows is in Table order, every live row present (Normalize).
	Rows []Row
}

// Session is one live session's report with its claim's role and issue: what Deployment.Sessions
// carries.
type Session struct {
	Role   claim.Role
	Issue  string
	Report Report
}

// MaxReportDetail is the longest detail a kept row carries, in bytes: a session's check quotes
// what it found, and a chatty failure is cut rather than kept whole in the store and the state.
const MaxReportDetail = 1024

// Normalize turns a wire report's rows into the rows the daemon keeps: a name Table has no live
// row for is dropped (returned in dropped, in wire order), each kept row is built by NewRow, a live
// row the wire lacks is appended as not OK with the detail "not reported by this session", and the
// kept rows are in Table order; a name sent twice keeps the first.
func Normalize(rows []claim.CapabilityRow) (kept []Row, dropped []string) {
	live := Live()
	byName := make(map[Name]Row, len(live))
	for _, row := range rows {
		name := Name(row.Name)
		if !slices.Contains(live, name) {
			dropped = append(dropped, row.Name)
			continue
		}
		if _, seen := byName[name]; seen {
			continue
		}
		byName[name] = NewRow(name, row.OK, row.Detail)
	}
	kept = make([]Row, 0, len(live))
	for _, name := range live {
		row, ok := byName[name]
		if !ok {
			row = Row{Name: name, Detail: "not reported by this session"}
		}
		kept = append(kept, row)
	}
	return kept, dropped
}

// cleanDetail is a reported detail as one printed line with no credential in it, cut at
// MaxReportDetail on a rune boundary. A detail quotes upstream errors and names the worked
// repository chooses (a server in `.mcp.json`, a file under `.omp/`): neither may split a log line,
// put a line of its own into the controller's instructions, or show a credential on the state.
//
// Every space rune (unicode.IsSpace: a newline, a tab, a next line, a Unicode space) is a space.
// Every other control rune (an escape, a C1 control) is replaced by held while credentials are
// redacted, then is a space too: a credential value runs on across held, since nothing delimited
// it there, so a control rune inside a credential never leaves part of it unredacted. Space runs
// then collapse to one space with none at either end. Redaction runs on the one line, so a
// credential a session split across lines is still found, and before the cut, so a cut never
// leaves a credential's head too short to match. The Legion plugin cleans its report the same way
// before it sends it (`cleanDetail` in `packages/pi-legion/src/capability-report.ts`), so
// cleaning is idempotent; both are held to `packages/contracts/fixtures/capability-redaction.json`.
func cleanDetail(text string) string {
	marked := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r):
			return held
		}
		return r
	}, text)
	redacted := strings.ReplaceAll(redactCredentials(marked), string(held), " ")
	return truncate(strings.Join(strings.Fields(redacted), " "), MaxReportDetail)
}

// held is what cleanDetail holds a control rune as while it redacts: a noncharacter, which no text
// carries, so it stands for nothing else.
const held = '\uFFFF'

// credentialRules are the credential shapes redactCredentials replaces, each keeping what names
// the credential and replacing the secret with `***`; the shared fixture names each with an
// example. They run on cleanDetail's line, where a space is the only whitespace and held stands for
// a control rune: a value class takes held, and a separator a space or held. Order matters: the
// Authorization header runs before the bare bearer rule, so a header under any scheme loses its
// whole value and keeps a listed scheme's label, and the GitHub rule runs before the JWT rule, so a
// stateless installation token (`ghs_<app id>_<JWT>`) reads `ghs_***`. A JWT starts only where a
// run of base64url characters starts, as the plugin's lookbehind has it: JavaScriptCore would
// otherwise read a long run once per `eyJ` in it. An Authorization value in quotes ends at a quote;
// one without runs to a closing bracket or the end of the line, since an AWS signature or a Digest
// credential carries commas, semicolons and quotes of its own. A rule whose value may be short
// takes `*` in its value class, so a value already redacted is matched whole and left as it is;
// the others need more than `***` to match at all.
var credentialRules = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^ /?#]+@`), "${1}***@"},
	{regexp.MustCompile(`(?i)([?&](?:token|key|secret|password|access_token|refresh_token|id_token|api_key|apikey|client_secret|sig|x-amz-security-token|x-amz-signature)=)[^ &#"'<>]+`), "${1}***"},
	{regexp.MustCompile(`(?i)(authorization["']?[ \x{FFFF}]*[:=][ \x{FFFF}]*)(?:(["'])((?:bearer|basic|token|digest)[ \x{FFFF}]+)?[^"']+|(\[?)((?:bearer|basic|token|digest)[ \x{FFFF}]+)?[^)\]}>]+)`), "${1}${2}${3}${4}${5}***"},
	{regexp.MustCompile(`(?i)(x-api-key["']?[ \x{FFFF}]*[:=][ \x{FFFF}]*["']?)[A-Za-z0-9._~+/=*\x{FFFF}-]+`), "${1}***"},
	{regexp.MustCompile(`(?i)\b(bearer[ \x{FFFF}]+)[A-Za-z0-9._~+/=*\x{FFFF}-]+`), "${1}***"},
	{regexp.MustCompile(`\b(gh[pousr]_|github_pat_)[A-Za-z0-9._\x{FFFF}-]{20,}`), "${1}***"},
	{regexp.MustCompile(`(^|[^A-Za-z0-9_\x{FFFF}-])eyJ[A-Za-z0-9_\x{FFFF}-]{8,}\.[A-Za-z0-9_\x{FFFF}-]{8,}\.[A-Za-z0-9_\x{FFFF}-]*`), "${1}***"},
	{regexp.MustCompile(`\b(sk-|dsp_)[A-Za-z0-9_\x{FFFF}-]{20,}`), "${1}***"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), "${1}***"},
}

// redactCredentials is text with every credentialRules match replaced, rule by rule.
func redactCredentials(text string) string {
	for _, rule := range credentialRules {
		text = rule.pattern.ReplaceAllString(text, rule.replacement)
	}
	return text
}

// truncate is text cut to at most max bytes on a rune boundary, so a cut never leaves a partial
// character behind.
func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
