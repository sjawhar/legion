import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { LegionCapabilityReport } from "@legion/contracts/legion-api";
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import {
  CAPABILITY_CHECK_TIMEOUT_MS,
  CAPABILITY_REPORT_TIMEOUT_MS,
  type CapabilityHost,
  type CapabilityInput,
  ENVOY_TOOL_NAMES,
  LIVE_CAPABILITIES,
  type LiveCapability,
  measureCapabilities,
  WEB_SEARCH_PROBE_QUERY,
} from "./capability-report";

interface RecordedRun {
  readonly command: string;
  readonly args: readonly string[];
  readonly cwd: string;
  readonly signal: AbortSignal;
}

interface RecordedSearch {
  readonly args: { readonly query: string; readonly limit: number };
  readonly options: { readonly sessionId?: string; readonly signal: AbortSignal };
}

/** A host where every check passes: three task agents, a provider answering the search, no MCP
 * server, nothing discovered, and both commands exiting 0. `overrides` replace members. */
function fakeHost(overrides: Partial<CapabilityHost> = {}): CapabilityHost & {
  readonly runs: RecordedRun[];
  readonly searches: RecordedSearch[];
} {
  const runs: RecordedRun[] = [];
  const searches: RecordedSearch[] = [];
  return {
    runs,
    searches,
    discoverAgents: async () => ({
      agents: [{ name: "oracle" }, { name: "deep-worker" }, { name: "scout" }],
    }),
    disabledAgents: async () => [],
    runSearchQuery: async (args, options) => {
      searches.push({ args, options });
      return {
        content: [{ type: "text", text: "[1] Jujutsu—a version control system" }],
        details: { response: { provider: "parallel", sources: [] } },
      };
    },
    loadMCPConfigs: async () => ({ configs: {}, sources: {} }),
    discoverMCPServers: async () => {
      throw new Error("discoverMCPServers must not run when no server is configured");
    },
    discoverExtensionPaths: async () => [],
    loadSkills: async () => ({ skills: [] }),
    run: async (command, args, options) => {
      runs.push({ command, args, cwd: options.cwd, signal: options.signal });
      return command === "gh"
        ? { code: 0, stdout: "octocat\n", stderr: "" }
        : { code: 0, stdout: "LEGION-663 · Capability report\n", stderr: "" };
    },
    ...overrides,
  };
}

let workspace: string;
const originalPath = process.env.PATH;

beforeAll(async () => {
  workspace = await mkdtemp(path.join(os.tmpdir(), "legion-capability-"));
});
afterAll(async () => {
  await rm(workspace, { recursive: true, force: true });
});
afterEach(() => {
  process.env.PATH = originalPath;
});

function input(overrides: Partial<CapabilityInput> = {}): CapabilityInput {
  return {
    cwd: workspace,
    sessionId: "ses_capability",
    issue: "LEGION-663",
    promptAgents: ["oracle", "deep-worker"],
    activeTools: ["read", "task", "web_search", ...ENVOY_TOOL_NAMES],
    agentsExposed: true,
    ...overrides,
  };
}

/** The named row of a report. */
async function measure(
  host: CapabilityHost,
  overrides: Partial<CapabilityInput> = {}
): Promise<Record<LiveCapability, { readonly ok: boolean; readonly detail: string }>> {
  const report = await measureCapabilities(host, input(overrides));
  const rows = {} as Record<LiveCapability, { readonly ok: boolean; readonly detail: string }>;
  for (const row of report.rows)
    rows[row.name as LiveCapability] = { ok: row.ok, detail: row.detail };
  return rows;
}

describe("the report", () => {
  test("carries the six live rows in the daemon's table order, when it started and how long it took, in the wire's shape", async () => {
    const before = Date.now();
    const report = await measureCapabilities(fakeHost(), input());
    const after = Date.now();

    expect(report.rows.map((row) => row.name)).toEqual([...LIVE_CAPABILITIES]);
    expect(report.rows.every((row) => row.ok)).toBe(true);
    const measuredAt = Date.parse(report.measuredAt);
    expect(measuredAt).toBeGreaterThanOrEqual(before);
    expect(measuredAt).toBeLessThanOrEqual(after);
    expect(Number.isInteger(report.elapsedMs)).toBe(true);
    expect(report.elapsedMs).toBeGreaterThanOrEqual(0);
    expect(report.elapsedMs).toBeLessThanOrEqual(after - measuredAt + 1);
    expect(LegionCapabilityReport.parse(report)).toEqual(report);
  });

  test("the budgets are 8 s a check and 10 s a report", () => {
    expect(CAPABILITY_CHECK_TIMEOUT_MS).toBe(8_000);
    expect(CAPABILITY_REPORT_TIMEOUT_MS).toBe(10_000);
  });

  test("ENVOY_TOOL_NAMES is every tool the Envoy plugin registers, in its order", () => {
    expect([...ENVOY_TOOL_NAMES]).toEqual(envoyToolSpecs.map((spec) => spec.name));
  });

  test("a check that throws is a failing row carrying the first line of its message; the others stand", async () => {
    const rows = await measure(
      fakeHost({
        discoverAgents: async () => {
          throw new Error("agents directory unreadable: EACCES\n    at readdir (node:fs)");
        },
      })
    );
    expect(rows.subagents).toEqual({ ok: false, detail: "agents directory unreadable: EACCES" });
    expect(rows["web-search"].ok).toBe(true);
    expect(rows.github.ok).toBe(true);
  });

  test("a host failing everywhere still yields six rows and never throws", async () => {
    const refuse = async (): Promise<never> => {
      throw new TypeError("host.discoverAgents is not a function");
    };
    const report = await measureCapabilities(
      {
        discoverAgents: refuse,
        disabledAgents: refuse,
        runSearchQuery: refuse,
        loadMCPConfigs: refuse,
        discoverMCPServers: refuse,
        discoverExtensionPaths: refuse,
        loadSkills: refuse,
        run: refuse,
      },
      input()
    );
    expect(report.rows).toHaveLength(6);
    expect(report.rows.map((row) => row.ok)).toEqual([false, false, false, true, false, false]);
    expect(report.rows[0]?.detail).toBe("host.discoverAgents is not a function");
  });

  test("a check still pending at its budget is a failing row and its signal is aborted", async () => {
    let seen: AbortSignal | undefined;
    const host = fakeHost({
      runSearchQuery: (_args, options) => {
        seen = options.signal;
        return Promise.withResolvers<never>().promise;
      },
    });
    const report = await measureCapabilities(host, input(), { checkMs: 20, reportMs: 500 });
    const row = report.rows.find((candidate) => candidate.name === "web-search");
    expect(row).toEqual({ name: "web-search", ok: false, detail: "did not finish within 20 ms" });
    expect(seen?.aborted).toBe(true);
    expect(report.rows.filter((candidate) => candidate.ok)).toHaveLength(5);
  });

  test("a row still pending at the report's budget is a failing row naming that budget", async () => {
    const host = fakeHost({ run: () => Promise.withResolvers<never>().promise });
    const report = await measureCapabilities(host, input(), { checkMs: 60, reportMs: 20 });
    const details = Object.fromEntries(report.rows.map((row) => [row.name, row.detail]));
    expect(details.github).toBe("did not finish within 20 ms");
    expect(details["dispatch-envoy-tools"]).toBe("did not finish within 20 ms");
    expect(report.rows.filter((row) => row.ok)).toHaveLength(4);
    expect(report.elapsedMs).toBeLessThan(60);
  });

  test("a whole-second budget reads in seconds", async () => {
    const host = fakeHost({ run: () => Promise.withResolvers<never>().promise });
    const report = await measureCapabilities(host, input(), { checkMs: 1_000, reportMs: 1_500 });
    const github = report.rows.find((row) => row.name === "github");
    expect(github).toEqual({ name: "github", ok: false, detail: "did not finish within 1 s" });
  });
});

describe("subagents", () => {
  test("passes when task resolves every agent the prompts dispatch", async () => {
    const rows = await measure(fakeHost());
    expect(rows.subagents).toEqual({
      ok: true,
      detail: "task resolves 3 agents, every one the prompts dispatch",
    });
  });

  test("fails when the host exposes no pi.agents", async () => {
    const rows = await measure(fakeHost(), { agentsExposed: false });
    expect(rows.subagents).toEqual({ ok: false, detail: "pi.agents is not exposed" });
  });

  test("fails when task is not an active tool", async () => {
    const rows = await measure(fakeHost(), { activeTools: ["read", "web_search"] });
    expect(rows.subagents).toEqual({ ok: false, detail: "task is not a registered tool" });
  });

  test("names the prompts' agents task does not resolve", async () => {
    const rows = await measure(fakeHost(), { promptAgents: ["oracle", "ghost", "phantom"] });
    expect(rows.subagents).toEqual({
      ok: false,
      detail: "agents the prompts dispatch that the task tool does not resolve: ghost, phantom",
    });
  });

  test("names the prompts' agents task.disabledAgents turns off", async () => {
    const rows = await measure(fakeHost({ disabledAgents: async () => ["oracle", "scout"] }));
    expect(rows.subagents).toEqual({
      ok: false,
      detail: "disabled by task.disabledAgents: oracle",
    });
  });

  test("reports both problems when both hold", async () => {
    const rows = await measure(fakeHost({ disabledAgents: async () => ["deep-worker"] }), {
      promptAgents: ["oracle", "deep-worker", "ghost"],
    });
    expect(rows.subagents).toEqual({
      ok: false,
      detail:
        "agents the prompts dispatch that the task tool does not resolve: ghost; disabled by task.disabledAgents: deep-worker",
    });
  });
});

describe("web-search", () => {
  test("runs the fixed probe query once, with the session id and the check's signal", async () => {
    const host = fakeHost();
    const rows = await measure(host);
    expect(rows["web-search"].ok).toBe(true);
    expect(rows["web-search"].detail).toMatch(
      /^web_search registered; provider parallel answered in \d+ ms$/
    );
    expect(host.searches).toHaveLength(1);
    expect(host.searches[0]?.args).toEqual({ query: WEB_SEARCH_PROBE_QUERY, limit: 1 });
    expect(WEB_SEARCH_PROBE_QUERY).toBe("jujutsu version control");
    expect(host.searches[0]?.options.sessionId).toBe("ses_capability");
    expect(host.searches[0]?.options.signal).toBeInstanceOf(AbortSignal);
  });

  test("fails without a search when web_search is not an active tool", async () => {
    const host = fakeHost();
    const rows = await measure(host, { activeTools: ["read", "task", ...ENVOY_TOOL_NAMES] });
    expect(rows["web-search"]).toEqual({
      ok: false,
      detail: "web_search is not a registered tool",
    });
    expect(host.searches).toEqual([]);
  });

  test("fails with the provider's error", async () => {
    const rows = await measure(
      fakeHost({
        runSearchQuery: async () => ({
          content: [],
          details: { error: "rate limited by exa", provider: "exa" },
        }),
      })
    );
    expect(rows["web-search"]).toEqual({
      ok: false,
      detail: "web_search failed: rate limited by exa",
    });
  });

  test("fails when no provider answered", async () => {
    for (const details of [{ provider: "none" }, { response: { provider: "none" } }, {}]) {
      const rows = await measure(
        fakeHost({
          runSearchQuery: async () => ({ content: [{ type: "text", text: "" }], details }),
        })
      );
      expect(rows["web-search"]).toEqual({
        ok: false,
        detail: "web_search failed: no provider answered",
      });
    }
  });

  test("fails with the first line of a text answer that is an error", async () => {
    const rows = await measure(
      fakeHost({
        runSearchQuery: async () => ({
          content: [{ type: "text", text: "Error: provider exa refused the key\nTry again later" }],
          details: { provider: "exa" },
        }),
      })
    );
    expect(rows["web-search"]).toEqual({
      ok: false,
      detail: "web_search failed: Error: provider exa refused the key",
    });
  });

  test("reads the provider from details.provider when the response names none", async () => {
    const rows = await measure(
      fakeHost({
        runSearchQuery: async () => ({
          content: [{ type: "text", text: "[1] Jujutsu" }],
          details: { provider: "brave" },
        }),
      })
    );
    expect(rows["web-search"].detail).toMatch(/^web_search registered; provider brave answered/);
  });
});

describe("mcp", () => {
  interface FakeManager {
    readonly statuses: Record<string, string>;
    readonly calls: string[];
    readonly waitError?: Error;
  }
  const manager = (fake: FakeManager) => ({
    waitForPendingConnections: async () => {
      fake.calls.push("wait");
      if (fake.waitError !== undefined) throw fake.waitError;
    },
    getConnectionStatus: (name: string) => fake.statuses[name] ?? "unknown",
    disconnectAll: async () => {
      fake.calls.push("disconnectAll");
    },
  });
  const configured = async () => ({
    configs: { alpha: { command: "alpha" }, beta: { url: "http://beta" } },
    sources: {
      alpha: { path: path.join(workspace, ".omp", "mcp.json") },
      beta: { path: path.join(workspace, ".mcp.json") },
    },
  });

  test("passes with no server configured, connecting nothing", async () => {
    const rows = await measure(fakeHost());
    expect(rows.mcp).toEqual({ ok: true, detail: "no MCP server configured" });
  });

  test("passes naming every connected server and the file it is configured in, then disconnects", async () => {
    const fake: FakeManager = { statuses: { alpha: "connected", beta: "connected" }, calls: [] };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({ manager: manager(fake), errors: [] }),
      })
    );
    expect(rows.mcp).toEqual({
      ok: true,
      detail: "connected: alpha (.omp/mcp.json), beta (.mcp.json)",
    });
    expect(fake.calls).toEqual(["wait", "disconnectAll"]);
  });

  test("fails naming the server that did not connect with the error recorded for it", async () => {
    const fake: FakeManager = { statuses: { alpha: "connected", beta: "disconnected" }, calls: [] };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({
          manager: manager(fake),
          errors: [
            { path: "mcp:beta", error: "ENOENT: no such file or directory, posix_spawn 'beta'" },
          ],
        }),
      })
    );
    expect(rows.mcp).toEqual({
      ok: false,
      detail:
        "not connected: beta (ENOENT: no such file or directory, posix_spawn 'beta'); connected: alpha (.omp/mcp.json)",
    });
    expect(fake.calls).toEqual(["wait", "disconnectAll"]);
  });

  test("fails with the connection status when no error names the server, and reads server/message entries", async () => {
    const fake: FakeManager = { statuses: { alpha: "failed" }, calls: [] };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({
          manager: manager(fake),
          errors: [{ server: "alpha", message: "handshake refused" }],
        }),
      })
    );
    expect(rows.mcp).toEqual({
      ok: false,
      detail: "not connected: alpha (handshake refused), beta (status unknown)",
    });
  });

  test("disconnects even when waiting for the connections throws, and the row carries the error", async () => {
    const fake: FakeManager = {
      statuses: {},
      calls: [],
      waitError: new Error("connection pool closed\nat MCPManager.wait"),
    };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({ manager: manager(fake), errors: [] }),
      })
    );
    expect(rows.mcp).toEqual({ ok: false, detail: "connection pool closed" });
    expect(fake.calls).toEqual(["wait", "disconnectAll"]);
  });

  test("disconnects when the check's budget aborts a connection that never finishes", async () => {
    const fake: FakeManager = { statuses: {}, calls: [] };
    const never = new Promise<void>(() => undefined);
    const stuck = {
      ...manager(fake),
      waitForPendingConnections: () => {
        fake.calls.push("wait");
        return never;
      },
    };
    const report = await measureCapabilities(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({ manager: stuck, errors: [] }),
      }),
      input(),
      { checkMs: 20, reportMs: 500 }
    );
    const mcp = report.rows.find((row) => row.name === "mcp");
    expect(mcp).toEqual({ name: "mcp", ok: false, detail: "did not finish within 20 ms" });
    // The abort tore the second set of clients down; the finally that never ran would have.
    expect(fake.calls).toEqual(["wait", "disconnectAll"]);
  });
});

describe("repository-extensions", () => {
  let repository: string;
  beforeAll(async () => {
    repository = path.join(workspace, "repository");
    await mkdir(path.join(repository, ".omp", "extensions", "dirext"), { recursive: true });
    await mkdir(path.join(repository, ".omp", "extensions", "notes"), { recursive: true });
    await mkdir(path.join(repository, ".omp", "skills", "alpha"), { recursive: true });
    await mkdir(path.join(repository, ".omp", "skills", "stray"), { recursive: true });
    await mkdir(path.join(repository, ".claude", "skills", "beta"), { recursive: true });
    await writeFile(path.join(repository, ".omp", "extensions", "flat.ts"), "export {};\n");
    await writeFile(path.join(repository, ".omp", "extensions", "README.md"), "# not a module\n");
    await writeFile(
      path.join(repository, ".omp", "extensions", "dirext", "index.ts"),
      "export {};\n"
    );
    await writeFile(
      path.join(repository, ".omp", "extensions", "notes", "notes.txt"),
      "no index\n"
    );
    await writeFile(path.join(repository, ".omp", "skills", "alpha", "SKILL.md"), "# alpha\n");
    await writeFile(path.join(repository, ".claude", "skills", "beta", "SKILL.md"), "# beta\n");
  });

  test("passes when the repository carries nothing to load, asking the host nothing", async () => {
    const empty = path.join(workspace, "empty");
    await mkdir(empty, { recursive: true });
    const rows = await measure(
      fakeHost({
        discoverExtensionPaths: async () => {
          throw new Error("must not be asked");
        },
      }),
      { cwd: empty }
    );
    expect(rows["repository-extensions"]).toEqual({
      ok: true,
      detail: "the repository carries no .omp/extensions, .omp/skills or .claude/skills",
    });
  });

  test("passes when every module and skill on disk is among what the host discovers", async () => {
    const rows = await measure(
      fakeHost({
        discoverExtensionPaths: async (cwd) => [
          path.join(cwd, ".omp", "extensions", "flat.ts"),
          path.join(cwd, ".omp", "extensions", "dirext", "index.ts"),
        ],
        loadSkills: async (cwd) => ({
          skills: [
            { filePath: path.join(cwd, ".omp", "skills", "alpha", "SKILL.md") },
            { filePath: path.join(cwd, ".claude", "skills", "beta", "SKILL.md") },
          ],
        }),
      }),
      { cwd: repository }
    );
    expect(rows["repository-extensions"]).toEqual({
      ok: true,
      detail:
        "discovered: .claude/skills/beta/SKILL.md, .omp/extensions/dirext, .omp/extensions/flat.ts, .omp/skills/alpha/SKILL.md",
    });
  });

  test("fails naming what is on disk and not discovered", async () => {
    const rows = await measure(
      fakeHost({
        discoverExtensionPaths: async (cwd) => [
          path.join(cwd, ".omp", "extensions", "dirext", "index.ts"),
        ],
        loadSkills: async (cwd) => ({
          skills: [{ filePath: path.join(cwd, ".omp", "skills", "alpha", "SKILL.md") }],
        }),
      }),
      { cwd: repository }
    );
    expect(rows["repository-extensions"]).toEqual({
      ok: false,
      detail:
        "not discovered: .claude/skills/beta/SKILL.md, .omp/extensions/flat.ts; discovered: .omp/extensions/dirext, .omp/skills/alpha/SKILL.md",
    });
  });
});

describe("dispatch-envoy-tools", () => {
  test("passes when the ten tools are active and dispatch read answers, naming PATH's dispatch", async () => {
    const bin = path.join(workspace, "bin");
    await mkdir(bin, { recursive: true });
    const dispatch = path.join(bin, "dispatch");
    await writeFile(dispatch, "#!/bin/sh\nexit 0\n");
    await chmod(dispatch, 0o755);
    process.env.PATH = `${path.join(workspace, "nowhere")}${path.delimiter}${bin}`;
    const host = fakeHost();

    const rows = await measure(host);
    expect(rows["dispatch-envoy-tools"]).toEqual({
      ok: true,
      detail: `ten Envoy tools registered; dispatch read LEGION-663 answered from ${dispatch}`,
    });
    const run = host.runs.find((candidate) => candidate.command === "dispatch");
    expect(run?.args).toEqual(["read", "--issue", "LEGION-663"]);
    expect(run?.cwd).toBe(workspace);
    expect(run?.signal).toBeInstanceOf(AbortSignal);
  });

  test("names dispatch itself when PATH has none", async () => {
    process.env.PATH = path.join(workspace, "nowhere");
    const rows = await measure(fakeHost());
    expect(rows["dispatch-envoy-tools"].detail).toBe(
      "ten Envoy tools registered; dispatch read LEGION-663 answered from dispatch"
    );
  });

  test("fails without running dispatch when Envoy tools are missing", async () => {
    const host = fakeHost();
    const rows = await measure(host, {
      activeTools: ["task", "web_search", ...ENVOY_TOOL_NAMES.slice(0, 8)],
    });
    expect(rows["dispatch-envoy-tools"]).toEqual({
      ok: false,
      detail: "Envoy tools missing: envoy_whoami, envoy_sessions",
    });
    expect(host.runs.map((run) => run.command)).toEqual(["gh"]);
  });

  test("fails with dispatch's first stderr line, else its exit code", async () => {
    const withStderr = await measure(
      fakeHost({
        run: async (command) =>
          command === "dispatch"
            ? { code: 1, stdout: "", stderr: "dispatch: issue LEGION-663 not found\nrun --help\n" }
            : { code: 0, stdout: "octocat\n", stderr: "" },
      })
    );
    expect(withStderr["dispatch-envoy-tools"]).toEqual({
      ok: false,
      detail: "dispatch read failed: dispatch: issue LEGION-663 not found",
    });
    const silent = await measure(
      fakeHost({
        run: async (command) =>
          command === "dispatch"
            ? { code: 2, stdout: "", stderr: "" }
            : { code: 0, stdout: "octocat\n", stderr: "" },
      })
    );
    expect(silent["dispatch-envoy-tools"]).toEqual({
      ok: false,
      detail: "dispatch read failed: exit code 2",
    });
  });
});

describe("github", () => {
  test("passes with the login gh api user answers", async () => {
    const host = fakeHost();
    const rows = await measure(host);
    expect(rows.github).toEqual({ ok: true, detail: "gh api user: octocat" });
    const run = host.runs.find((candidate) => candidate.command === "gh");
    expect(run?.args).toEqual(["api", "user", "--jq", ".login"]);
    expect(run?.cwd).toBe(workspace);
  });

  test("fails with gh's first stderr line, else its exit code", async () => {
    const withStderr = await measure(
      fakeHost({
        run: async (command) =>
          command === "gh"
            ? {
                code: 1,
                stdout: "",
                stderr: "gh: HTTP 401: Bad credentials (https://api.github.com/user)\n",
              }
            : { code: 0, stdout: "", stderr: "" },
      })
    );
    expect(withStderr.github).toEqual({
      ok: false,
      detail: "gh api user failed: gh: HTTP 401: Bad credentials (https://api.github.com/user)",
    });
    const silent = await measure(
      fakeHost({
        run: async (command) =>
          command === "gh"
            ? { code: 4, stdout: "", stderr: "" }
            : { code: 0, stdout: "", stderr: "" },
      })
    );
    expect(silent.github).toEqual({ ok: false, detail: "gh api user failed: exit code 4" });
  });

  test("passes under a GitHub App installation token, the only credential a Legion role holds", async () => {
    // A role's gh runs on its App's installation token (appauth: `legion-implementer[bot]`,
    // `legion-reviewer[bot]`), and GitHub answers REST `GET /user` to an installation token with
    // 403 "Resource not accessible by integration" — read in a Legion pod on 2026-10-10 through
    // the reviewer App's token. GraphQL `{viewer{login}}` is how such a token reads its own
    // identity, and how the repository's proofs read it (internal/api/real_github_test.go,
    // scripts/e2e/stage4b-sandbox-tree.sh), so the row passes with the bot login it answers.
    const installationToken = async (command: string, args: readonly string[]) => {
      if (command !== "gh") return { code: 0, stdout: "", stderr: "" };
      if (
        args[0] === "api" &&
        args[1] === "graphql" &&
        args.some((arg) => arg.includes("viewer"))
      ) {
        return { code: 0, stdout: "legion-reviewer[bot]\n", stderr: "" };
      }
      return {
        code: 1,
        stdout: "",
        stderr: "gh: Resource not accessible by integration (HTTP 403)\n",
      };
    };
    const rows = await measure(fakeHost({ run: installationToken }));
    expect(rows.github.ok).toBe(true);
    expect(rows.github.detail).toContain("legion-reviewer[bot]");
  });
});
