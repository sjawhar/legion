import { readdir, stat } from "node:fs/promises";
import path from "node:path";
import type { LegionCapabilityReportBody, LegionCapabilityRow } from "@legion/contracts/legion-api";
import { hasErrnoCode, messageFor } from "@legion/envoy-client/errors";

/**
 * The six live capability rows a Legion session measures as it boots and reports with
 * `claims/ready` (LEGION-663): what the daemon's `capabilities` table calls `subagents`,
 * `web-search`, `mcp`, `repository-extensions`, `dispatch-envoy-tools` and `github`. Each check
 * proves the capability from inside the session, the way the agent would use it: the `task` tool
 * resolving the agents the role's prompts dispatch, one real web search, the configured MCP servers
 * connecting, the repository's own extensions and skills loaded, the ten Envoy tools registered and
 * `dispatch read` answering, and the role's App login through `gh api graphql {viewer{login}}`.
 *
 * The daemon renders the report and never refuses a session for it, so a failing check is a
 * failing row, never a stopped boot: `measureCapabilities` never throws. Every row's detail leaves
 * cleaned (`cleanDetail`), since a check quotes upstream errors and the report reaches the
 * daemon's unauthenticated state route. Every collaborator a check needs is behind
 * `CapabilityHost`, so a test fakes it; `ompCapabilityHost()` is the production one.
 */

/** The budget of one check; a check still pending at it is a failing row. */
export const CAPABILITY_CHECK_TIMEOUT_MS = 8_000;
/** The budget of the whole report; a row still pending at it is a failing row. */
export const CAPABILITY_REPORT_TIMEOUT_MS = 10_000;
/** The one query the web-search check runs, against a subject every provider answers. */
export const WEB_SEARCH_PROBE_QUERY = "jujutsu version control";

/** The Envoy tools `@sjawhar/pi-envoy` registers (`envoyToolSpecs` in
 * `@legion/envoy-client/tool-contract`), each of which the dispatch-envoy-tools check expects
 * among the session's active tools. */
export const ENVOY_TOOL_NAMES = [
  "envoy_subscribe",
  "envoy_unsubscribe",
  "envoy_list",
  "envoy_inbox",
  "envoy_send",
  "envoy_publish",
  "envoy_role_set",
  "envoy_role_get",
  "envoy_whoami",
  "envoy_sessions",
] as const;

/** The live rows, in the daemon's table order (`internal/capabilities`, `Live()`). */
export const LIVE_CAPABILITIES = [
  "subagents",
  "web-search",
  "mcp",
  "repository-extensions",
  "dispatch-envoy-tools",
  "github",
] as const;

export type LiveCapability = (typeof LIVE_CAPABILITIES)[number];

/** What one web search answered, as far as the check reads it. */
export interface SearchQueryResult {
  readonly content?: readonly { readonly type: string; readonly text?: string }[];
  readonly details?: unknown;
}

/** The slice of Oh My Pi's MCP server manager the mcp check drives. */
export interface MCPServerManager {
  waitForPendingConnections(): Promise<void>;
  getConnectionStatus(name: string): string;
  disconnectAll(): Promise<void>;
}

/** What a finished child command left: its exit code (null when a signal ended it), and both
 * streams in full. */
export interface CommandResult {
  readonly code: number | null;
  readonly stdout: string;
  readonly stderr: string;
}

/**
 * Every collaborator a check needs: Oh My Pi's own discovery and search functions, its settings,
 * and the two child commands. The production host (`ompCapabilityHost`) reaches each lazily; a
 * test fakes the whole object. Every member answers a promise, since the production ones load
 * their module first.
 */
export interface CapabilityHost {
  /** `discoverAgents(cwd)`: the task agents the `task` tool resolves in this workspace. */
  readonly discoverAgents: (
    cwd: string
  ) => Promise<{ readonly agents: readonly { readonly name: string }[] }>;
  /** The `task.disabledAgents` setting (`cfgTaskDisabledAgents.get(settings)`). */
  readonly disabledAgents: () => Promise<readonly string[]>;
  /** `runSearchQuery`: one web search through the session's configured provider. */
  readonly runSearchQuery: (
    args: { readonly query: string; readonly limit: number },
    options: { readonly sessionId?: string; readonly signal: AbortSignal }
  ) => Promise<SearchQueryResult>;
  /** `loadAllMCPConfigs(cwd)`: every configured MCP server by name, and the file each came from. */
  readonly loadMCPConfigs: (cwd: string) => Promise<{
    readonly configs: Readonly<Record<string, unknown>>;
    readonly sources: Readonly<Record<string, { readonly path: string }>>;
  }>;
  /** `discoverMCPServers(cwd)`: a manager connecting to every configured server, and the errors
   * the connections raised. */
  readonly discoverMCPServers: (
    cwd: string
  ) => Promise<{ readonly manager: MCPServerManager; readonly errors: readonly unknown[] }>;
  /** `discoverExtensionPaths([], cwd, disabled)`: the module path of every extension Oh My Pi
   * would load for this workspace. */
  readonly discoverExtensionPaths: (cwd: string) => Promise<readonly string[]>;
  /** `loadSkills({...skills settings, cwd, disabledExtensions})`: every skill it loads. */
  readonly loadSkills: (
    cwd: string
  ) => Promise<{ readonly skills: readonly { readonly filePath: string }[] }>;
  /** One child command resolved on PATH, run in `cwd`, ended by `signal`. */
  readonly run: (
    command: string,
    args: readonly string[],
    options: { readonly cwd: string; readonly signal: AbortSignal }
  ) => Promise<CommandResult>;
}

/** What the checks measure against: the pane's workspace and session, the claim's issue, the
 * agents the role's prompts dispatch (the registration's `promptAgents`), and the host's tool
 * surface as `session_start` sees it. */
export interface CapabilityInput {
  readonly cwd: string;
  readonly sessionId: string;
  readonly issue: string;
  readonly promptAgents: readonly string[];
  readonly activeTools: readonly string[];
  /** Whether the host exposes `pi.agents`, the extension API the `task` tool's agents live behind. */
  readonly agentsExposed: boolean;
}

/** The two budgets; a test shortens them. */
export interface CapabilityBudgets {
  readonly checkMs: number;
  readonly reportMs: number;
}

const DEFAULT_BUDGETS: CapabilityBudgets = {
  checkMs: CAPABILITY_CHECK_TIMEOUT_MS,
  reportMs: CAPABILITY_REPORT_TIMEOUT_MS,
};

interface Outcome {
  readonly ok: boolean;
  readonly detail: string;
}

type Check = (
  host: CapabilityHost,
  input: CapabilityInput,
  signal: AbortSignal
) => Promise<Outcome>;

const ok = (detail: string): Outcome => ({ ok: true, detail });
const failed = (detail: string): Outcome => ({ ok: false, detail });

/** The first line of `text`, without surrounding blank lines. */
function firstLine(text: string): string {
  return text.trim().split("\n", 1)[0] ?? "";
}

/** A command's failure as its first stderr line, else its exit code. */
function commandFailure(result: CommandResult): string {
  return firstLine(result.stderr) || `exit code ${result.code ?? "none"}`;
}

/** The first executable `name` on `process.env.PATH`, or `name` itself when none is. */
function whichOnPath(name: string): string {
  return Bun.which(name, { PATH: process.env.PATH ?? "" }) ?? name;
}

const checkSubagents: Check = async (host, input) => {
  if (!input.agentsExposed) return failed("pi.agents is not exposed");
  if (!input.activeTools.includes("task")) return failed("task is not a registered tool");
  const [discovered, disabled] = await Promise.all([
    host.discoverAgents(input.cwd),
    host.disabledAgents(),
  ]);
  const names = new Set(discovered.agents.map((agent) => agent.name));
  const missing = input.promptAgents.filter((agent) => !names.has(agent));
  const off = input.promptAgents.filter((agent) => disabled.includes(agent));
  const problems: string[] = [];
  if (missing.length > 0) {
    problems.push(
      `agents the prompts dispatch that the task tool does not resolve: ${missing.join(", ")}`
    );
  }
  if (off.length > 0) problems.push(`disabled by task.disabledAgents: ${off.join(", ")}`);
  if (problems.length > 0) return failed(problems.join("; "));
  return ok(`task resolves ${names.size} agents, every one the prompts dispatch`);
};

const checkWebSearch: Check = async (host, input, signal) => {
  if (!input.activeTools.includes("web_search")) {
    return failed("web_search is not a registered tool");
  }
  const started = Date.now();
  const result = await host.runSearchQuery(
    { query: WEB_SEARCH_PROBE_QUERY, limit: 1 },
    { sessionId: input.sessionId, signal }
  );
  const elapsed = Date.now() - started;
  const details = (result.details ?? {}) as {
    readonly error?: unknown;
    readonly provider?: unknown;
    readonly response?: { readonly provider?: unknown };
  };
  if (details.error !== undefined && details.error !== null) {
    return failed(`web_search failed: ${String(details.error)}`);
  }
  const provider = details.response?.provider ?? details.provider;
  if (provider === undefined || provider === null || provider === "none") {
    return failed("web_search failed: no provider answered");
  }
  const text = result.content?.[0]?.text ?? "";
  if (text.startsWith("Error")) return failed(`web_search failed: ${firstLine(text)}`);
  return ok(`web_search registered; provider ${String(provider)} answered in ${elapsed} ms`);
};

/** The error `discoverMCPServers` recorded for `name`, when its `errors` carry one. Oh My Pi
 * records `{path: "mcp:<name>", error}`; a `server` or `name` member and a `message` are read the
 * same way, and anything else naming the server is quoted whole. */
function mcpErrorFor(errors: readonly unknown[], name: string): string | undefined {
  for (const entry of errors) {
    if (typeof entry !== "object" || entry === null) {
      if (!String(entry).includes(name)) continue;
      return String(entry);
    }
    const record = entry as Record<string, unknown>;
    const names = [record.server, record.name, record.path];
    if (!names.some((value) => value === name || value === `mcp:${name}`)) continue;
    const message = record.error ?? record.message;
    return message === undefined ? String(entry) : String(message);
  }
  return undefined;
}

const checkMCP: Check = async (host, input, signal) => {
  const { configs, sources } = await host.loadMCPConfigs(input.cwd);
  const configured = Object.keys(configs);
  if (configured.length === 0) return ok("no MCP server configured");
  const { manager, errors } = await host.discoverMCPServers(input.cwd);
  // The session's own MCP clients are the host's; these are a second, short-lived set, torn down
  // when the check ends and when its budget aborts it, so a server that never finishes connecting
  // leaves no client running for the session's lifetime. The listener covers an abort after the
  // manager exists; the line after it, one that came while the manager was still being made,
  // which no listener added since can hear.
  let torn: Promise<void> | undefined;
  const tearDown = () => {
    torn ??= manager.disconnectAll().catch(() => undefined);
    return torn;
  };
  signal.addEventListener("abort", tearDown, { once: true });
  if (signal.aborted) void tearDown();
  try {
    await manager.waitForPendingConnections();
    const connected: string[] = [];
    const unconnected: string[] = [];
    for (const name of configured) {
      const status = manager.getConnectionStatus(name);
      if (status === "connected") {
        const source = sources[name]?.path;
        connected.push(
          source === undefined ? name : `${name} (${path.relative(input.cwd, source)})`
        );
      } else {
        unconnected.push(`${name} (${mcpErrorFor(errors, name) ?? `status ${status}`})`);
      }
    }
    if (unconnected.length === 0) return ok(`connected: ${connected.join(", ")}`);
    const suffix = connected.length === 0 ? "" : `; connected: ${connected.join(", ")}`;
    return failed(`not connected: ${unconnected.join(", ")}${suffix}`);
  } finally {
    signal.removeEventListener("abort", tearDown);
    await tearDown();
  }
};

const EXTENSION_MODULE = /\.(?:ts|js|mjs|cjs)$/;
const EXTENSION_INDEXES = ["index.ts", "index.js", "index.mjs", "index.cjs"];
const SKILL_DIRECTORIES = [path.join(".omp", "skills"), path.join(".claude", "skills")];

/** The names in `directory`, or none where there is no such directory. */
async function namesIn(directory: string): Promise<readonly string[]> {
  try {
    return await readdir(directory);
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT") || hasErrnoCode(error, "ENOTDIR")) return [];
    throw error;
  }
}

async function kindOf(file: string): Promise<"file" | "directory" | undefined> {
  try {
    const stats = await stat(file);
    return stats.isFile() ? "file" : stats.isDirectory() ? "directory" : undefined;
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT") || hasErrnoCode(error, "ENOTDIR")) return undefined;
    throw error;
  }
}

interface RepositoryEntry {
  /** Absolute. */
  readonly path: string;
  /** A directory extension: discovered when a discovered module path is inside it. */
  readonly directory: boolean;
}

/** What the repository carries for Oh My Pi to load: `.omp/extensions/` modules (a file, or a
 * directory with an index), and the `SKILL.md` of each `.omp/skills/*` and `.claude/skills/*`. */
async function repositoryEntries(cwd: string): Promise<RepositoryEntry[]> {
  const entries: RepositoryEntry[] = [];
  const extensions = path.join(cwd, ".omp", "extensions");
  for (const name of await namesIn(extensions)) {
    const candidate = path.join(extensions, name);
    const kind = await kindOf(candidate);
    if (kind === "file" && EXTENSION_MODULE.test(name)) {
      entries.push({ path: candidate, directory: false });
    } else if (kind === "directory") {
      const indexes = await Promise.all(
        EXTENSION_INDEXES.map((index) => kindOf(path.join(candidate, index)))
      );
      if (indexes.includes("file")) entries.push({ path: candidate, directory: true });
    }
  }
  for (const skills of SKILL_DIRECTORIES) {
    for (const name of await namesIn(path.join(cwd, skills))) {
      const skill = path.join(cwd, skills, name, "SKILL.md");
      if ((await kindOf(skill)) === "file") entries.push({ path: skill, directory: false });
    }
  }
  return entries;
}

const checkRepositoryExtensions: Check = async (host, input) => {
  const entries = await repositoryEntries(input.cwd);
  if (entries.length === 0) {
    return ok("the repository carries no .omp/extensions, .omp/skills or .claude/skills");
  }
  const [extensionPaths, loaded] = await Promise.all([
    host.discoverExtensionPaths(input.cwd),
    host.loadSkills(input.cwd),
  ]);
  const discovered = new Set(
    [...extensionPaths, ...loaded.skills.map((skill) => skill.filePath)].map((file) =>
      path.resolve(file)
    )
  );
  const isDiscovered = (entry: RepositoryEntry): boolean => {
    const resolved = path.resolve(entry.path);
    if (discovered.has(resolved)) return true;
    if (!entry.directory) return false;
    const prefix = resolved + path.sep;
    return [...discovered].some((file) => file.startsWith(prefix));
  };
  const relative = (selected: RepositoryEntry[]): string =>
    selected
      .map((entry) => path.relative(input.cwd, entry.path))
      .sort()
      .join(", ");
  const found: RepositoryEntry[] = [];
  const lost: RepositoryEntry[] = [];
  for (const entry of entries) (isDiscovered(entry) ? found : lost).push(entry);
  if (lost.length === 0) return ok(`discovered: ${relative(found)}`);
  const suffix = found.length === 0 ? "" : `; discovered: ${relative(found)}`;
  return failed(`not discovered: ${relative(lost)}${suffix}`);
};

const checkDispatchEnvoyTools: Check = async (host, input, signal) => {
  const missing = ENVOY_TOOL_NAMES.filter((tool) => !input.activeTools.includes(tool));
  if (missing.length > 0) return failed(`Envoy tools missing: ${missing.join(", ")}`);
  const result = await host.run("dispatch", ["read", "--issue", input.issue], {
    cwd: input.cwd,
    signal,
  });
  if (result.code !== 0) return failed(`dispatch read failed: ${commandFailure(result)}`);
  return ok(
    `ten Envoy tools registered; dispatch read ${input.issue} answered from ${whichOnPath("dispatch")}`
  );
};

/** The role's own GitHub identity through GraphQL `viewer`, never REST `GET /user`: a role's `gh`
 * runs on its App's installation token, which GitHub answers `GET /user` to with 403 "Resource not
 * accessible by integration", and `{viewer{login}}` is how such a token reads its own identity —
 * the read the repository's proofs make (`internal/api/real_github_test.go`,
 * `scripts/e2e/stage4b-sandbox-tree.sh`). */
const checkGitHub: Check = async (host, input, signal) => {
  const result = await host.run(
    "gh",
    ["api", "graphql", "-f", "query={viewer{login}}", "--jq", ".data.viewer.login"],
    { cwd: input.cwd, signal }
  );
  if (result.code !== 0) return failed(`gh viewer login failed: ${commandFailure(result)}`);
  return ok(`gh viewer login: ${result.stdout.trim()}`);
};

const CHECKS: Readonly<Record<LiveCapability, Check>> = {
  subagents: checkSubagents,
  "web-search": checkWebSearch,
  mcp: checkMCP,
  "repository-extensions": checkRepositoryExtensions,
  "dispatch-envoy-tools": checkDispatchEnvoyTools,
  github: checkGitHub,
};

/** The longest detail a row carries, in UTF-8 bytes: the daemon's `capabilities.MaxReportDetail`. */
const MAX_DETAIL_BYTES = 1024;

/** What `cleanDetail` holds a control character as while it redacts, what it holds U+009B as, and
 * what its second reading holds a control sequence's introducer as: the daemon's `held`,
 * `c1Introducer` and `opener`. */
const HELD = "\uffff";
const C1_INTRODUCER = "\ufffe";
const OPENER = "\ufdd0";

/** `cleanDetail`'s readings of held text after the first, in order: the daemon's `heldReadings`,
 * whose second takes no `.` as an intermediate, since it separates a JWT's segments. */
const HELD_READINGS: readonly (readonly [RegExp, string])[] = [
  [/(?:\uffff\[|\ufffe)[0-?]*[!-)+,/-]*/g, OPENER],
  [/\ufdd0[@-~]?/g, HELD],
  [/\uffff/g, " "],
];

/** `C1_INTRODUCER`, `OPENER`, held alone, or a held escape with the `[`, parameters and final byte
 * that complete a control sequence: the daemon's `heldSequence`, as regular-expression source. */
const HELD_SEQUENCE = String.raw`\ufffe|\ufdd0|\uffff(?:\[[0-?]*[@-~])?`;

/** `HELD_SEQUENCE` without `OPENER`: what a JWT's first segment runs across, the daemon's
 * `jwtFirstHeldSequence`. */
const JWT_FIRST_HELD_SEQUENCE = String.raw`\ufffe|\uffff(?:\[[0-?]*[@-~])?`;

/** Go's `unicode.IsSpace`, the space runes the daemon's cleanup splits on. */
function isSpace(code: number): boolean {
  return (
    (code >= 0x09 && code <= 0x0d) ||
    code === 0x20 ||
    code === 0x85 ||
    code === 0xa0 ||
    code === 0x1680 ||
    (code >= 0x2000 && code <= 0x200a) ||
    code === 0x2028 ||
    code === 0x2029 ||
    code === 0x202f ||
    code === 0x205f ||
    code === 0x3000
  );
}

/** `text` cut to at most `max` UTF-8 bytes on a character boundary. */
function truncateUtf8(text: string, max: number): string {
  let bytes = 0;
  let end = 0;
  for (const char of text) {
    const code = char.codePointAt(0) ?? 0;
    bytes += code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4;
    if (bytes > max) break;
    end += char.length;
  }
  return text.slice(0, end);
}

/**
 * The credential shapes `cleanDetail` replaces, as the daemon's `credentialRules` in
 * `packages/daemon/internal/capabilities` and in its order, which its comment explains; the shared
 * fixture names each with an example. A value class runs across a `HELD_SEQUENCE`, and a separator
 * takes a space or `HELD`. The userinfo rule starts only where a run of scheme characters starts.
 * The JWT rule starts nowhere its first segment's class could have run on from: not after held, a
 * C1 introducer or a value character, and not after a held `[` and its parameters, followed by any
 * final byte or by a `\uXXXX` escape whose backslash is that final byte. Its one lookbehind for the
 * held prefix reads like the daemon's rule, which matches that prefix with `eyJ` and gives it back.
 * It may start after an opener, which its first segment does not run across, and right after a
 * percent or `\u` escape that no held sequence closes. It may also start right after held when its
 * first segment crosses no held character and no opener, only a C1 introducer, and right after a
 * held `[`, its parameters and an optional final byte or `\uXXXX` when its first segment holds a C1
 * introducer and crosses nothing else held, as the daemon's comment explains. These segments stop
 * at the next held character, and a first segment ends at its one dot, so no start reads another's
 * run: JavaScriptCore backtracks, where the daemon's RE2 does not, and a start inside a run its
 * first segment reads through would read that run once per start.
 */
const CREDENTIAL_RULES: readonly (readonly [RegExp, string])[] = [
  [/(?<![a-z0-9+.-])([0-9+.-]*[a-z][a-z0-9+.-]*:\/\/)[^ /?#]+@/gi, "$1***@"],
  [
    /([?&](?:token|key|secret|password|access_token|accesstoken|refresh_token|id_token|auth_token|private_token|api_key|apikey|client_secret|sig|signature|x-amz-security-token|x-amz-signature|x-goog-signature)=)[^ &#"'<>]+/gi,
    "$1***",
  ],
  [
    /\b(password[ \ufffe\uffff]*=[ \ufffe\uffff]*)(?:(')(?:[^'\\]|\\.)*('?)|[^ &#"'<>]+)/gi,
    "$1$2***$3",
  ],
  [
    /(authorization\\?["']?[ \ufffe\uffff]*[:=][ \ufffe\uffff]*)(?:(\\["'](?:(?:bearer|basic|token|digest)[ \ufffe\uffff]+)?)[^"'\\]+|(["'](?:(?:bearer|basic|token|digest)[ \ufffe\uffff]+)?)(?:[^"'\\]|\\.)+|(\[?(?:(?:bearer|basic|token|digest)[ \ufffe\uffff]+)?)[^)\]}>]+)/gi,
    "$1$2$3$4***",
  ],
  [
    new RegExp(
      String.raw`((?:api-key|ocp-apim-subscription-key|x-subscription-token)\\?["']?[ \ufffe\uffff]*[:=][ \ufffe\uffff]*\\?["']?)(?:[A-Za-z0-9._~+/=*-]|${HELD_SEQUENCE})+`,
      "gi"
    ),
    "$1***",
  ],
  [
    /(\\?["'](?:api_key|access_token)\\?["'][ \ufffe\uffff]*:[ \ufffe\uffff]*\\?["'])[^"'\\]+/gi,
    "$1***",
  ],
  [
    new RegExp(
      String.raw`\b(bearer[ \ufffe\uffff]+)(?:[A-Za-z0-9._~+/=*-]|${HELD_SEQUENCE})+`,
      "gi"
    ),
    "$1***",
  ],
  [
    new RegExp(String.raw`\b(gh[pousr]_|github_pat_)(?:[A-Za-z0-9._-]|${HELD_SEQUENCE}){20,}`, "g"),
    "$1***",
  ],
  [
    new RegExp(
      String.raw`(?:(?=eyJ)(?<!\uffff\[[0-?]*(?:[@-~]?|\\u[0-9A-Fa-f]{4}))(?:(?<![A-Za-z0-9_\ufffe\uffff-])|(?<=%[0-9A-Fa-f]{2}|\\u[0-9A-Fa-f]{4}))eyJ(?:[A-Za-z0-9_-]|${JWT_FIRST_HELD_SEQUENCE}){8,}|(?<=\uffff)eyJ(?:[A-Za-z0-9_-]|\ufffe){8,}|(?=eyJ)(?<=\uffff\[[0-?]*(?:[@-~]|\\u[0-9A-Fa-f]{4})?)eyJ[A-Za-z0-9_-]*\ufffe(?:[A-Za-z0-9_-]|\ufffe)*)\.(?:[A-Za-z0-9_-]|${HELD_SEQUENCE}){8,}\.(?:[A-Za-z0-9_-]|${HELD_SEQUENCE})*`,
      "g"
    ),
    "***",
  ],
  [
    new RegExp(
      String.raw`\b(sk-|dsp_|ya29\.|xox[a-z]-|glpat-|npm_)(?:[A-Za-z0-9_-]|${HELD_SEQUENCE}){20,}`,
      "g"
    ),
    "$1***",
  ],
  [new RegExp(String.raw`\b(AIza)(?:[A-Za-z0-9_-]|${HELD_SEQUENCE}){35,}`, "g"), "$1***"],
  [new RegExp(String.raw`\b(AKIA|ASIA)(?:(?:${HELD_SEQUENCE})*[A-Z0-9]){16}\b`, "g"), "$1***"],
];

/** The most passes `redactCredentials` makes: the daemon's `maxRedactionPasses`. */
const MAX_REDACTION_PASSES = 8;

/** `text` with every `CREDENTIAL_RULES` match replaced, rule by rule, pass after pass until a pass
 * changes nothing (at most `MAX_REDACTION_PASSES`), as the daemon's `redactCredentials` does. */
function redactCredentials(text: string): string {
  let line = text;
  for (let pass = 0; pass < MAX_REDACTION_PASSES; pass++) {
    const next = CREDENTIAL_RULES.reduce(
      (redacted, [pattern, replacement]) => redacted.replace(pattern, replacement),
      line
    );
    if (next === line) break;
    line = next;
  }
  return line;
}

/** `text` without its last code point, a surrogate pair's two halves together. */
function withoutLastCodePoint(text: string): string {
  const last = text.charCodeAt(text.length - 1);
  return text.slice(0, last >= 0xdc00 && last <= 0xdfff && text.length > 1 ? -2 : -1);
}

/**
 * A detail as the daemon's `cleanDetail` (`packages/daemon/internal/capabilities`) keeps it, in its
 * steps, which its comment explains, and held to the same shared fixture: one line, every
 * credential redacted, cut at 1024 UTF-8 bytes. Every space rune is a space, U+009B is held as
 * `C1_INTRODUCER`, and every other control character (and each marker itself) is held; the rules
 * run on that, then, when anything was held, on each of `HELD_READINGS` in turn, each time until a
 * pass changes nothing. After a cut, trailing space goes, then trailing words, and within a last
 * word trailing characters, while the rules would still change what is left, but never the last
 * character; so cleaning what this returns gives it back unchanged, and a cut never empties a
 * detail that had text. The daemon cleans the report again, which leaves it as it is; this copy
 * keeps a credential inside the pod when the daemon is a contract-21 release from before that
 * cleaning (legion-v11.5.0, v11.5.1).
 */
export function cleanDetail(text: string): string {
  let marked = "";
  for (const char of text) {
    const code = char.codePointAt(0) ?? 0;
    // Go's unicode.IsControl (C0, DEL and C1), the three markers themselves, and the replacement
    // character an invalid UTF-8 byte decodes to.
    const control =
      code < 0x20 ||
      (code >= 0x7f && code < 0xa0) ||
      code === 0xfffe ||
      code === 0xfdd0 ||
      code === 0xfffd;
    marked += isSpace(code) ? " " : code === 0x9b ? C1_INTRODUCER : control ? HELD : char;
  }
  let redacted = redactCredentials(marked);
  if (marked.includes(HELD) || marked.includes(C1_INTRODUCER)) {
    for (const [pattern, replacement] of HELD_READINGS) {
      const read = redacted.replace(pattern, replacement);
      if (read !== redacted) redacted = redactCredentials(read);
    }
  }
  const line = redacted
    .split(" ")
    .filter((field) => field !== "")
    .join(" ");
  let cut = truncateUtf8(line, MAX_DETAIL_BYTES).replace(/ +$/, "");
  if (cut.length < line.length) {
    while (redactCredentials(cut) !== cut) {
      const space = cut.lastIndexOf(" ");
      cut = space > 0 ? cut.slice(0, space) : withoutLastCodePoint(cut);
    }
  }
  return cut;
}

/** `8 s` for a whole number of seconds, else the milliseconds. */
function budgetLabel(ms: number): string {
  return ms % 1_000 === 0 ? `${ms / 1_000} s` : `${ms} ms`;
}

/** `work`'s answer, or `onDeadline()` once `ms` have passed without one. */
async function withinBudget<T>(work: Promise<T>, ms: number, onDeadline: () => T): Promise<T> {
  const deadline = Promise.withResolvers<T>();
  const timer = setTimeout(() => deadline.resolve(onDeadline()), ms);
  try {
    return await Promise.race([work, deadline.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Measures the six live rows concurrently: each check under `budgets.checkMs` (its signal is
 * aborted at the deadline), the whole report under `budgets.reportMs`. A check that throws, or
 * is still pending at its deadline, is a failing row with the reason; the function itself never
 * throws. Every row's detail is cleaned here (`cleanDetail`), the one place every check's outcome
 * passes, so no branch of a check quotes a credential into the report. `measuredAt` is when the
 * measuring started, `elapsedMs` how long it took, and the rows are in `LIVE_CAPABILITIES` order.
 */
export async function measureCapabilities(
  host: CapabilityHost,
  input: CapabilityInput,
  budgets: CapabilityBudgets = DEFAULT_BUDGETS
): Promise<LegionCapabilityReportBody> {
  const started = Date.now();
  const measuredAt = new Date(started).toISOString();
  const outcomes = new Map<LiveCapability, Outcome>();
  const controllers: AbortController[] = [];
  const checks = LIVE_CAPABILITIES.map(async (name) => {
    const controller = new AbortController();
    controllers.push(controller);
    const outcome = await withinBudget(
      CHECKS[name](host, input, controller.signal).catch((error: unknown) =>
        failed(firstLine(messageFor(error)))
      ),
      budgets.checkMs,
      () => {
        controller.abort();
        return failed(`did not finish within ${budgetLabel(budgets.checkMs)}`);
      }
    );
    outcomes.set(name, outcome);
  });
  await withinBudget(
    Promise.all(checks).then(() => undefined),
    budgets.reportMs,
    () => undefined
  );
  for (const controller of controllers) controller.abort();
  const rows: LegionCapabilityRow[] = LIVE_CAPABILITIES.map((name) => {
    const { ok, detail } =
      outcomes.get(name) ?? failed(`did not finish within ${budgetLabel(budgets.reportMs)}`);
    return { name, ok, detail: cleanDetail(detail) };
  });
  return { measuredAt, elapsedMs: Date.now() - started, rows };
}

/** `command` run from PATH in `cwd`, ended by `signal`, with both streams read in full. */
async function runCommand(
  command: string,
  args: readonly string[],
  options: { readonly cwd: string; readonly signal: AbortSignal }
): Promise<CommandResult> {
  const child = Bun.spawn([command, ...args], {
    cwd: options.cwd,
    env: process.env,
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    signal: options.signal,
  });
  const [stdout, stderr] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code: child.exitCode, stdout, stderr };
}

/**
 * The production host: Oh My Pi's own modules, and the pane's `dispatch` and `gh`.
 *
 * Every `@oh-my-pi/pi-coding-agent` import below is a lazy, string-literal `import()`, and must
 * stay one: the package and its subpaths exist only inside Oh My Pi's bundle, whose loader
 * resolves a bare specifier of it only when written as a string literal (a variable specifier
 * fails with "Cannot find package"), and a top-level import of a subpath would fail `bun test`
 * and every non-Legion session this entry loads in, which never measure. Loaded here, the
 * modules are reached only inside a Legion session, at measurement time.
 */
export function ompCapabilityHost(): CapabilityHost {
  const hostSettings = async () =>
    (await import("@oh-my-pi/pi-coding-agent/config/settings")).settings;
  return {
    discoverAgents: async (cwd) => (await import("@oh-my-pi/pi-coding-agent")).discoverAgents(cwd),
    disabledAgents: async () => {
      const [{ cfgTaskDisabledAgents }, settings] = await Promise.all([
        import("@oh-my-pi/pi-coding-agent/task/settings"),
        hostSettings(),
      ]);
      return cfgTaskDisabledAgents.get(settings);
    },
    runSearchQuery: async (args, options) =>
      (await import("@oh-my-pi/pi-coding-agent")).runSearchQuery(args, options),
    loadMCPConfigs: async (cwd) =>
      (await import("@oh-my-pi/pi-coding-agent/mcp/config")).loadAllMCPConfigs(cwd),
    discoverMCPServers: async (cwd) =>
      (await import("@oh-my-pi/pi-coding-agent")).discoverMCPServers(cwd),
    discoverExtensionPaths: async (cwd) => {
      const [{ discoverExtensionPaths }, { cfgDisabledExtensions }, settings] = await Promise.all([
        import("@oh-my-pi/pi-coding-agent/extensibility/extensions/loader"),
        import("@oh-my-pi/pi-coding-agent/extensibility/settings"),
        hostSettings(),
      ]);
      return discoverExtensionPaths([], cwd, cfgDisabledExtensions.get(settings));
    },
    loadSkills: async (cwd) => {
      const [{ loadSkills }, { cfgDisabledExtensions, cfgSkills }, settings] = await Promise.all([
        import("@oh-my-pi/pi-coding-agent"),
        import("@oh-my-pi/pi-coding-agent/extensibility/settings"),
        hostSettings(),
      ]);
      return loadSkills({
        ...cfgSkills.get(settings),
        cwd,
        disabledExtensions: cfgDisabledExtensions.get(settings),
      });
    },
    run: runCommand,
  };
}
