// Package capabilities is the one list of what a Legion worker can do (LEGION-578: every worker is
// a full agent). Each capability is checked at one site — in the image by `legion probe-image`
// (CheckImage), by a live check against a running pod, by the daemon from the deployment's
// configuration, or withheld by a ruling — and the probe prints the whole table, one line per row,
// so an operator reads a single declared list in the probe pod's log rather than inferring what
// was checked from which probes happened to run.
package capabilities

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
	// SiteLive is to be proved by a live check against a running pod: it needs a cluster, a
	// model, or a service the image alone cannot show. No such check runs yet; the row says so.
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
	// live check proves it (may be ""); "" otherwise.
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
	{Subagents, SiteLive, "dispatches task subagents, each on the model its role configures", "", ""},
	{EvalJS, SiteImage, "evaluates JavaScript in Oh My Pi's own runtime", "", ""},
	{EvalPython, SiteImage, "evaluates Python through the interpreter `omp setup python` manages", "", ""},
	{Browser, SiteImage, "drives a headless Chromium through the browser tools", "", ""},
	{LSP, SiteImage, "reads diagnostics and symbols from the Go, TypeScript and Python language servers", "", ""},
	{CodeGraph, SiteImage, "queries the CodeGraph index for affected tests, impact and callers", "", ""},
	{WebSearch, SiteLive, "searches the web through the web-search tool", "", ""},
	{Skills, SiteImage, "loads the skills Legion's prompts name", "", ""},
	{MCP, SiteLive, "reaches the MCP servers the deployment configures", "", ""},
	{RepositoryExtensions, SiteLive, "loads the Oh My Pi extensions the repository it works carries", "dispatch://LEGION-629", ""},
	{DispatchEnvoyTools, SiteLive, "reaches Dispatch and Envoy through the pi-envoy tools", "", ""},
	{GitHub, SiteLive, "reads and writes GitHub through `legion gh` on its role's App", "dispatch://LEGION-631", ""},
	{Secrets, SiteDeployment, "reads the secrets the deployment grants its pod generation through the agent-secrets broker", "", ""},
	{ModelFallback, SiteDeployment, "falls back to another model when its own is unavailable (retry.modelFallback)", "", ""},
	{Toolchain, SiteImage, "builds and runs with the generic toolchain: go, curl, wget, python3, node, bun and uv", "", ""},
	{ResourceLimits, SiteDeployment, "runs within the CPU and memory the deployment's pod resources give it", "", ""},
	{Network, SiteWithheld, "reaches beyond the pod's own network: the pod is the boundary", "dispatch://LEGION-5", ""},
	{OperatorSetup, SiteWithheld, "installs or configures its own dependencies in the pod: Legion owns its dependencies", "dispatch://LEGION-200", ""},
	{ProductionIdentities, SiteWithheld, "acts under a production identity of its own", "dispatch://LEGION-551, dispatch://LEGION-205", ""},
}

// Decidable is the SiteDeployment names in Table order: what a legion.yaml decided line may name,
// and what the daemon measures from its deployment (Deployment.Report).
func Decidable() []Name {
	var names []Name
	for _, row := range Table {
		if row.Site == SiteDeployment {
			names = append(names, row.Name)
		}
	}
	return names
}
