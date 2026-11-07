import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { chmod, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  LegionCapabilityReport,
  type LegionCapabilityReportBody,
} from "@legion/contracts/legion-api";
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import {
  CAPABILITY_CHECK_TIMEOUT_MS,
  CAPABILITY_REPORT_TIMEOUT_MS,
  type CapabilityHost,
  type CapabilityInput,
  type CommandResult,
  cleanDetail,
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
        ? { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" }
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

/**
 * The names of the rows that passed, less `repository-extensions`: alone among the live checks it
 * awaits the filesystem — a `readdir`, then a `stat` for each entry — so its outcome lands on a
 * macrotask a loaded event loop can defer past a short budget, the budget working as designed, not
 * a defect. `checkDispatchEnvoyTools` reads the disk too, but synchronously, through `whichOnPath`
 * (`Bun.which`); every other check settles in microtasks alone under `fakeHost`, so no timer can
 * defer one and its row reads the same on any machine.
 */
function passingNamesExceptTheDiskCheck(report: LegionCapabilityReportBody): string[] {
  return report.rows
    .filter((row) => row.ok && row.name !== "repository-extensions")
    .map((row) => row.name);
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
    expect(passingNamesExceptTheDiskCheck(report)).toEqual([
      "subagents",
      "mcp",
      "dispatch-envoy-tools",
      "github",
    ]);
  });

  test("a row still pending at the report's budget is a failing row naming that budget, and the report does not wait for it", async () => {
    const pending = Promise.withResolvers<CommandResult>();
    const host = fakeHost({ run: () => pending.promise });
    // Nothing but a budget can end the two command rows, and the check budget is six times the
    // per-test limit below: a report that ends them on its own 20 ms budget comes back at once,
    // while one that waited for its checks could not come back inside that limit at all, so the
    // limit is the hang guard this row's proof rests on.
    const report = await measureCapabilities(host, input(), { checkMs: 30_000, reportMs: 20 });
    // The report is back: resolving lets the abandoned checks settle, so their 30 s timers clear.
    pending.resolve({ code: 0, stdout: "", stderr: "" });

    for (const name of ["github", "dispatch-envoy-tools"])
      expect(report.rows.find((row) => row.name === name)).toEqual({
        name,
        ok: false,
        detail: "did not finish within 20 ms",
      });
    expect(passingNamesExceptTheDiskCheck(report)).toEqual(["subagents", "web-search", "mcp"]);
  }, 5_000);

  test("a whole-second budget reads in seconds", async () => {
    const host = fakeHost({ run: () => Promise.withResolvers<never>().promise });
    const report = await measureCapabilities(host, input(), { checkMs: 1_000, reportMs: 1_500 });
    const github = report.rows.find((row) => row.name === "github");
    expect(github).toEqual({ name: "github", ok: false, detail: "did not finish within 1 s" });
  });

  // INFRA-927: the session measures again while a row is missing (src/capability-schedule.ts), so
  // a second measurement must carry nothing of the first: the row that failed on its budget is
  // asked again, and so is every row that passed.
  test("a second measurement carries nothing of the first: every row is measured again, and the one that failed on its budget passes", async () => {
    let searches = 0;
    const host = fakeHost({
      // The first measurement's search never answers, so its row fails on the check budget; the
      // second's answers.
      runSearchQuery: () => {
        searches += 1;
        return searches === 1
          ? Promise.withResolvers<never>().promise
          : Promise.resolve({
              content: [{ type: "text", text: "[1] Jujutsu—a version control system" }],
              details: { response: { provider: "parallel" } },
            });
      },
    });
    const budgets = { checkMs: 20, reportMs: 500 };

    const first = await measureCapabilities(host, input(), budgets);
    const second = await measureCapabilities(host, input(), budgets);

    expect(first.rows.find((row) => row.name === "web-search")).toEqual({
      name: "web-search",
      ok: false,
      detail: "did not finish within 20 ms",
    });
    expect(second.rows.map((row) => row.name)).toEqual([...LIVE_CAPABILITIES]);
    expect(passingNamesExceptTheDiskCheck(second)).toEqual([
      "subagents",
      "web-search",
      "mcp",
      "dispatch-envoy-tools",
      "github",
    ]);
    // The second report is its own measurement, not the first with one row patched: it started
    // after the first ended, and every check asked the host a second time.
    expect(Date.parse(second.measuredAt)).toBeGreaterThan(Date.parse(first.measuredAt));
    expect(first.elapsedMs).toBeGreaterThanOrEqual(20);
    expect(searches).toBe(2);
    // `dispatch` and `gh`, once each per measurement, in the order the checks start them.
    expect(host.runs.map((run) => run.command)).toEqual(["dispatch", "gh", "dispatch", "gh"]);
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

  test("redacts the userinfo of a URL the recorded error quotes", async () => {
    const fake: FakeManager = { statuses: { alpha: "connected", beta: "failed" }, calls: [] };
    const rows = await measure(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => ({
          manager: manager(fake),
          errors: [
            {
              path: "mcp:beta",
              error: "fetch failed: https://user:secret@mcp.example/sse (HTTP 401)",
            },
          ],
        }),
      })
    );
    expect(rows.mcp).toEqual({
      ok: false,
      detail:
        "not connected: beta (fetch failed: https://***@mcp.example/sse (HTTP 401)); connected: alpha (.omp/mcp.json)",
    });
  });

  test("disconnects when the budget aborted while the manager was still being made", async () => {
    const fake: FakeManager = { statuses: {}, calls: [] };
    const tornDown = Promise.withResolvers<void>();
    const stuck = {
      ...manager(fake),
      waitForPendingConnections: () => {
        fake.calls.push("wait");
        return new Promise<void>(() => undefined);
      },
      disconnectAll: async () => {
        fake.calls.push("disconnectAll");
        tornDown.resolve();
      },
    };
    // The manager exists only once the report has settled, which aborts every check's signal —
    // the command checks', read here — and the report settles once the mcp check's own 20 ms
    // budget has aborted it. A listener the check adds after that never hears the abort.
    const settled = Promise.withResolvers<void>();
    const report = await measureCapabilities(
      fakeHost({
        loadMCPConfigs: configured,
        discoverMCPServers: async () => {
          await settled.promise;
          return { manager: stuck, errors: [] };
        },
        run: async (_command, _args, options) => {
          options.signal.addEventListener("abort", () => settled.resolve(), { once: true });
          return { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" };
        },
      }),
      input(),
      { checkMs: 20, reportMs: 500 }
    );
    const mcp = report.rows.find((row) => row.name === "mcp");
    expect(mcp).toEqual({ name: "mcp", ok: false, detail: "did not finish within 20 ms" });
    // The check found its signal already aborted and tore the clients down before waiting on a
    // connection that never finishes; the finally that never runs would have.
    await tornDown.promise;
    expect(fake.calls).toEqual(["disconnectAll", "wait"]);
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
  test("passes when the eleven tools are active and dispatch read answers, naming PATH's dispatch", async () => {
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
      detail: `11 Envoy tools registered; dispatch read LEGION-663 answered from ${dispatch}`,
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
      "11 Envoy tools registered; dispatch read LEGION-663 answered from dispatch"
    );
  });

  test("fails without running dispatch when Envoy tools are missing", async () => {
    const host = fakeHost();
    const rows = await measure(host, {
      activeTools: ["task", "web_search", ...ENVOY_TOOL_NAMES.slice(0, 8)],
    });
    expect(rows["dispatch-envoy-tools"]).toEqual({
      ok: false,
      detail: "Envoy tools missing: envoy_whoami, envoy_sessions, slack_post",
    });
    expect(host.runs.map((run) => run.command)).toEqual(["gh"]);
  });

  test("fails with dispatch's first stderr line, else its exit code", async () => {
    const withStderr = await measure(
      fakeHost({
        run: async (command) =>
          command === "dispatch"
            ? { code: 1, stdout: "", stderr: "dispatch: issue LEGION-663 not found\nrun --help\n" }
            : { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" },
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
            : { code: 0, stdout: "legion-implementer[bot]\n", stderr: "" },
      })
    );
    expect(silent["dispatch-envoy-tools"]).toEqual({
      ok: false,
      detail: "dispatch read failed: exit code 2",
    });
  });
});

describe("github", () => {
  test("passes with the login the viewer query answers", async () => {
    const host = fakeHost();
    const rows = await measure(host);
    expect(rows.github).toEqual({ ok: true, detail: "gh viewer login: legion-implementer[bot]" });
    const run = host.runs.find((candidate) => candidate.command === "gh");
    expect(run?.args).toEqual([
      "api",
      "graphql",
      "-f",
      "query={viewer{login}}",
      "--jq",
      ".data.viewer.login",
    ]);
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
                stderr: "gh: HTTP 401: Bad credentials (https://api.github.com/graphql)\n",
              }
            : { code: 0, stdout: "", stderr: "" },
      })
    );
    expect(withStderr.github).toEqual({
      ok: false,
      detail:
        "gh viewer login failed: gh: HTTP 401: Bad credentials (https://api.github.com/graphql)",
    });
    const silent = await measure(
      fakeHost({
        run: async (command) =>
          command === "gh"
            ? { code: 4, stdout: "", stderr: "" }
            : { code: 0, stdout: "", stderr: "" },
      })
    );
    expect(silent.github).toEqual({ ok: false, detail: "gh viewer login failed: exit code 4" });
  });

  test("passes under a GitHub App installation token, the only credential a Legion role holds", async () => {
    // A role's gh runs on its App's installation token (appauth: the App's `<slug>[bot]`), and
    // GitHub answers REST `GET /user` to an installation token with 403 "Resource not accessible
    // by integration" — read in a Legion pod on 2026-10-10 through the reviewer App's token.
    // GraphQL `{viewer{login}}` is how such a token reads its own identity, and how the
    // repository's proofs read it (internal/api/real_github_test.go,
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

/** The cases both funnels are held to: `cleanDetail` here and the daemon's `capabilities.NewRow`.
 * `patterns` is a detail per credential shape or cleanup rule and what it reads cleaned, each
 * prefixed with `pad` letters `a` where it has one; `branches` is each branch of a check that quotes
 * upstream text, with that text, the credential in it, and the text redacted; `linear`, which only
 * this test reads since the daemon's RE2 cannot backtrack, is a unit that, repeated, would make a
 * backtracking engine read a run once per start in it. */
interface RedactionFixture {
  readonly patterns: readonly {
    readonly pattern: string;
    readonly name: string;
    readonly pad?: number;
    readonly detail: string;
    readonly want: string;
  }[];
  readonly branches: readonly {
    readonly name: string;
    readonly row: LiveCapability;
    readonly upstream: string;
    readonly credential: string;
    readonly redacted: string;
  }[];
  readonly linear: readonly { readonly name: string; readonly unit: string }[];
}

const fixtures = path.resolve(import.meta.dir, "../../contracts/fixtures");
const redaction = JSON.parse(
  readFileSync(path.join(fixtures, "capability-redaction.json"), "utf8")
) as RedactionFixture;

/** The report rows this plugin sends for the fixture's branches, which the daemon's
 * `TestNormalizeKeepsThePluginsReportAsItIs` runs through `Normalize`. `UPDATE_FIXTURES=1` rewrites
 * it from what the plugin sends now. */
const PLUGIN_REPORT = path.join(fixtures, "capability-redaction-plugin-report.json");

describe("cleanDetail", () => {
  for (const { pattern, name, pad = 0, detail, want } of redaction.patterns) {
    test(`${pattern}: ${name}`, () => {
      const padding = "a".repeat(pad);
      expect(cleanDetail(padding + detail)).toBe(padding + want);
      // The daemon cleans what this plugin sends again, so cleaning is idempotent.
      expect(cleanDetail(padding + want)).toBe(padding + want);
    });
  }

  // One test per case, as above, so each case's placements (its length plus 25, at most 1,033) have
  // Bun's per-test budget to themselves; every case's together are about ten thousand.
  for (const { pattern, name, pad, detail } of redaction.patterns) {
    test(`at the cut keeps what came before it and is idempotent: ${pattern}: ${name}, each byte cut once`, () => {
      const problems: string[] = [];
      const whole = cleanDetail(detail);
      for (
        let letters = Math.max(0, 1024 - Buffer.byteLength(detail) - 16);
        letters <= 1024 + 8;
        letters++
      ) {
        const padding = "a".repeat(letters);
        const once = cleanDetail(`${padding} ${detail}`);
        const label = `pushed by ${letters} letters`;
        if (cleanDetail(once) !== once) problems.push(`${label}: not idempotent`);
        if (!once.startsWith(padding.slice(0, 1024))) problems.push(`${label}: lost its padding`);
        if (pad === undefined && !`${padding} ${whole}`.startsWith(once)) {
          problems.push(`${label}: not a prefix of the case cleaned whole`);
        }
      }
      expect(problems).toEqual([]);
    });
  }

  // A rule that may start inside a run its own value class reads through makes JavaScriptCore,
  // which backtracks, read that run once per start: time quadratic in the detail, which the plugin
  // cleans before it cuts it to 1,024 bytes, so one such detail stalls a session's start. Each
  // fixture unit, repeated, holds a start for every repetition. Measured from 4 to 128 KiB, every
  // case here doubles its cost when its text doubles, below and above one step where the engine
  // leaves a fast path and the cost jumps about 40x in a single doubling; a quadratic rule would
  // read four times the cost at every doubling instead. That step falls at a different size for
  // each case, so at the 64 KiB this row cleans two cases land just above it and take about 369 and
  // 383 ms on an idle machine.
  //
  // What this row holds is that `cleanDetail` comes back on the largest input the fixture builds,
  // inside the hang guard below. What it does not hold is that a rule has stayed linear: a
  // quadratic rule is not caught here. The JWT rule made quadratic by hand costs 13,570 to
  // 14,088 ms on a 2-CPU gVisor agent pod, which the 10 s guard does fail; but CI is about 4.5
  // times faster (job 128346885451 ran the two cases above at 83.20 and 80.76 ms), where that same
  // regression would cost about 3 s and pass — an estimate from those ratios, not a run on CI. One
  // bound at 64 KiB cannot hold both machines: it would have to sit above the 2,633 ms a linear
  // rule costs on that pod under load and below that 3 s, which is no room to stand on.
  // INFRA-985's architect ruled on 2026-11-07 that this is not fixed here: an accepted risk of this
  // change, recorded in the pull request body, not an oversight.
  for (const { name, unit } of redaction.linear) {
    test(`cleans 64 KiB of ${name} without stalling`, () => {
      const text = unit.repeat(Math.ceil((64 * 1024) / unit.length));
      cleanDetail(text);
      // The limit is a hang guard: on bun 1.4.4 a per-test limit is reported for a body that
      // blocks the thread only once that body comes back, so it ends a blow-up that returns at
      // all, and a rule that never returns is ended by the pi-legion job's own timeout-minutes: 10.
    }, 10_000);
  }
});

describe("credential redaction", () => {
  test("every row passes cleanDetail: each branch that quotes upstream text carries it redacted", async () => {
    const upstream = (row: LiveCapability): string => {
      const branch = redaction.branches.find((candidate) => candidate.row === row);
      if (branch === undefined) throw new Error(`the fixture has no branch on the ${row} row`);
      return branch.upstream;
    };
    const report = await measureCapabilities(
      fakeHost({
        runSearchQuery: async () => ({ content: [], details: { error: upstream("web-search") } }),
        discoverAgents: async () => {
          throw new Error(`${upstream("subagents")}\n    at discoverAgents`);
        },
        run: async (command) =>
          command === "dispatch"
            ? { code: 1, stdout: "", stderr: `${upstream("dispatch-envoy-tools")}\nsecond line\n` }
            : { code: 1, stdout: "", stderr: `${upstream("github")}\n` },
        loadMCPConfigs: async () => ({
          configs: { alpha: { url: "https://mcp.example/sse" } },
          sources: {},
        }),
        discoverMCPServers: async () => ({
          manager: {
            waitForPendingConnections: async () => undefined,
            getConnectionStatus: () => upstream("mcp"),
            disconnectAll: async () => undefined,
          },
          errors: [],
        }),
      }),
      input()
    );

    const details = Object.fromEntries(report.rows.map((row) => [row.name, row.detail]));
    for (const { name, row, redacted } of redaction.branches) {
      expect({ name, detail: details[row] }).toEqual({
        name,
        detail: expect.stringContaining(redacted),
      });
    }
    const wire = JSON.stringify(report);
    for (const { credential } of redaction.branches) expect(wire).not.toContain(credential);

    const sent = { rows: report.rows };
    if (process.env.UPDATE_FIXTURES === "1") {
      writeFileSync(PLUGIN_REPORT, `${JSON.stringify(sent, null, 2)}\n`);
    }
    expect(JSON.parse(readFileSync(PLUGIN_REPORT, "utf8"))).toEqual(sent);
  });
});
