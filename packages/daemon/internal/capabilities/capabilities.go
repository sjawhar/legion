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
// store and the state each keep a shape of their own.
type Row struct {
	Name Name `json:"name"`
	// OK is whether the session's check of the row passed.
	OK bool `json:"ok"`
	// Detail is the fact the check found, passed or not, as one line of at most MaxReportDetail
	// bytes with every credential it quoted redacted (cleanDetail): it reaches the daemon log, the
	// store, the unauthenticated state route and the controller's daily report with nothing
	// delimiting it, so no newline or control character the session sent survives, and no
	// credential an upstream error quoted does either.
	// A row that is not OK always has one — "no detail reported" when the session sent none — since
	// the state's `open[].detail` is a non-empty string; an OK row's may be empty, a passing check
	// with nothing to add.
	Detail string `json:"detail"`
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
// row for is dropped (returned in dropped, in wire order), a detail is cleaned to one line with its
// credentials redacted and cut at MaxReportDetail (cleanDetail), a row that is not OK with no
// detail left reads "no detail reported", a live row the wire lacks is appended as not OK with the
// detail "not reported by this session", and the kept rows are in Table order; a name sent twice
// keeps the first. The ready route is its one caller, so every detail the daemon keeps from a
// session — what it logs, persists and shows on the state — has passed cleanDetail.
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
		detail := cleanDetail(row.Detail)
		if !row.OK && detail == "" {
			detail = "no detail reported"
		}
		byName[name] = Row{Name: name, OK: row.OK, Detail: detail}
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

// cleanDetail is a reported detail as one printed line: every control rune — a newline, a tab,
// an escape — is a space, whitespace runs are one space with none at either end, every credential
// is redacted (redactCredentials), and the line is cut at MaxReportDetail on a rune boundary. A
// detail quotes names the worked repository chooses (a server in `.mcp.json`, a file under
// `.omp/`), and a name must neither split a log line nor put a line of its own into the
// controller's instructions. Redaction runs on the one line, so a credential split by control
// characters is still found, and before the cut, so a cut never leaves a credential's head too
// short to match.
func cleanDetail(text string) string {
	spaced := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	return truncate(redactCredentials(strings.Join(strings.Fields(spaced), " ")), MaxReportDetail)
}

// credentialPatterns are the credential shapes a check's upstream error may quote, each with what
// replaces it, applied in order: a URL's userinfo (`scheme://user:password@`, `scheme://user@`, up
// to the last `@` before the host); the value of a `token`, `key`, `secret`, `password`,
// `access_token` or `sig` query parameter; an Authorization or Proxy-Authorization header's
// credential, after its scheme when the scheme is bearer, basic, token or digest; a bearer
// credential anywhere; a GitHub token (`ghp_`, `gho_`, `ghs_`, `ghu_`, `ghr_`, `github_pat_`); and
// an AWS access key id (`AKIA`, `ASIA`). Each keeps what names the credential and replaces the
// secret with `***`. The Legion plugin redacts its report with the same list before it sends it
// (`packages/pi-legion/src/capability-report.ts`), and both are held to
// `packages/contracts/fixtures/capability-redaction.json`.
var credentialPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/?#]+@`), "${1}***@"},
	{regexp.MustCompile(`(?i)([?&](?:token|key|secret|password|access_token|sig)=)[^&\s#"'<>]+`), "${1}***"},
	{regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?(?:(?:bearer|basic|token|digest)\s+)?)[A-Za-z0-9._~+/=-]+`), "${1}***"},
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]+`), "${1}***"},
	{regexp.MustCompile(`\b(gh[pousr]_)[A-Za-z0-9]{20,}`), "${1}***"},
	{regexp.MustCompile(`\b(github_pat_)[A-Za-z0-9_]{20,}`), "${1}***"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), "${1}***"},
}

// redactCredentials is text with every credentialPatterns match replaced.
func redactCredentials(text string) string {
	for _, credential := range credentialPatterns {
		text = credential.pattern.ReplaceAllString(text, credential.replacement)
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
