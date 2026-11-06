import { afterAll, afterEach, beforeAll, beforeEach, expect, mock, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path, { join } from "node:path";
import { agentSubject, roleToken } from "@legion/contracts";
import {
  HEARTBEAT_RETRY_MAX_MS,
  HEARTBEAT_RETRY_MIN_MS,
} from "@legion/envoy-client/envoy-settings";
import type { ZodNumberProperty } from "@legion/pi-shared/pi-types";
import { captureEnvironment, type EnvironmentRestore } from "@legion/test-fixtures/environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type envoyEntryDefault from "../../pi-envoy/extensions/envoy";
import type { resetNatsConnectForTests as ResetNatsConnectForTests } from "../../pi-envoy/extensions/envoy";
import type legionEntryDefault from "./legion";

/** The daemon's golden registration answer (`packages/daemon/internal/api`), so a field the daemon
 * adds to it reaches the stub below. */
const registered: Record<string, unknown> = JSON.parse(
  readFileSync(
    path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
    "utf8"
  )
);

// The dial the Envoy entry opens NATS with, handed to it through `setNatsConnectForTests` once
// that entry is loaded below. The `nats` module itself stays as it is, so
// `@legion/envoy-client/nats-connection` resolves the NATS credential with the real nkey exports,
// and every other file in this `bun test` process reads the real module. The dial is not the
// module: the setter writes a slot of the process, not of one entry instance, so the `afterAll`
// below puts that slot back with `resetNatsConnectForTests` and no later file dials this
// stand-in.
const natsStandInConnect = async () => ({
  isClosed: () => false,
  close: async () => undefined,
  drain: async () => undefined,
  flush: async () => undefined,
  subscribe: () => ({
    unsubscribe: () => undefined,
    [Symbol.asyncIterator]: async function* () {
      await new Promise<never>(() => undefined);
    },
  }),
});

import { hostAgentRegistryMock } from "@legion/pi-shared/test/host-registry";

mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
  ...hostAgentRegistryMock,
}));
// The Envoy entry picks its envoy.json when its module loads, and both entries read it on each use:
// a file that holds no section, so the panes here take Dispatch and Envoy from the variables the
// test sets, never from the caller's own envoy.json or Oh My Pi profile. The directory's path is
// reserved here and the directory is made, the file written and both entries loaded in the
// `beforeAll` below: a `bun test -t <pattern>` that selects no row of this file runs no hook of
// it, and everything the module body did there would outlive the run.
const envoyConfigDirectory = temporaryDirectory("legion-role-claim-envoy-config");
const envoyConfigFile = join(envoyConfigDirectory, "envoy.json");

/** The Envoy and Legion entries, each loaded once the environment below is in place. The one row
 *  of this file reads them from its own body, and nothing starts a pane before the `beforeAll`
 *  that fills them. */
let envoyExtension: typeof envoyEntryDefault;
let legionExtension: typeof legionEntryDefault;
let resetNatsConnectForTests: typeof ResetNatsConnectForTests | undefined;
let restoreEntryEnvironment: EnvironmentRestore | undefined;

beforeAll(async () => {
  writeFileSync(envoyConfigFile, "{}\n");
  // Every name a pane this file starts writes, captured above the import that loads the Envoy
  // entry, so the restore puts back what the caller had rather than what the entry wrote: the
  // three the entry's module scope writes as it loads — `PATH`, which it puts its own `bin/`
  // first on, `DISPATCH_HOST`, and the `ENVOY_CONFIG_FILE` it settles on — the two deleted below
  // so no profile of the caller's picks another envoy.json, and `DISPATCH_SESSION_ID`, which the
  // entry writes for the session a pane here binds. They go back when this file ends: `bun test`
  // runs every file of a package in one process, and the next file reads the environment this one
  // leaves. They go back rather than being deleted, since the baseline is "not a Legion pane" and
  // a process with no `PATH` finds none of the programs these tests run.
  restoreEntryEnvironment = captureEnvironment([
    "ENVOY_CONFIG_FILE",
    "OMP_PROFILE",
    "PI_CODING_AGENT_DIR",
    "PATH",
    "DISPATCH_HOST",
    "DISPATCH_SESSION_ID",
  ]);
  process.env.ENVOY_CONFIG_FILE = envoyConfigFile;
  delete process.env.OMP_PROFILE;
  delete process.env.PI_CODING_AGENT_DIR;
  // Distinct mtime queries force distinct module instances, exercising the
  // cross-module role-claim bridge documented in envoy.ts. The Envoy entry is the sibling plugin's,
  // reached by path: this test is the one place the two entries meet in one process. Its NATS dial
  // is the stand-in above, handed over as soon as the entry is loaded and before any pane here
  // starts a session. The slot that dial goes in is the process's, so the set and the reset below
  // are one pair of hooks: a run that selects no row of this file does neither, where a set at
  // module scope would leave the next file of the same run dialling this stand-in.
  const envoyEntry = await import("../../pi-envoy/extensions/envoy.ts?envoy-entry");
  envoyExtension = envoyEntry.default;
  resetNatsConnectForTests = envoyEntry.resetNatsConnectForTests;
  envoyEntry.setNatsConnectForTests(natsStandInConnect);
  const legionEntry = await import("./legion.ts?legion-entry");
  legionExtension = legionEntry.default;
  // The boot's capability report measures through this host rather than the production one, which
  // reaches Oh My Pi's own modules and runs the pane's `gh` and `dispatch`; what the rows say is
  // `legion.test.ts`'s to pin.
  legionEntry.setLegionCapabilityHostForTests(() => ({
    discoverAgents: async () => ({ agents: [] }),
    disabledAgents: async () => [],
    runSearchQuery: async () => ({ content: [], details: {} }),
    loadMCPConfigs: async () => ({ configs: {}, sources: {} }),
    discoverMCPServers: async () => {
      throw new Error("no MCP server is configured in this test");
    },
    discoverExtensionPaths: async () => [],
    loadSkills: async () => ({ skills: [] }),
    run: async () => ({ code: 1, stdout: "", stderr: "no command runs in this test" }),
  }));
});

afterAll(() => {
  restoreEntryEnvironment?.();
  // The next file of this `bun test` run gets nats.js's own `connect` back.
  resetNatsConnectForTests?.();
});
type Context = {
  readonly cwd: string;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionFile: () => string | undefined;
    readonly ensureOnDisk: () => Promise<void>;
    readonly getEntries: () => readonly unknown[];
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly setTimeout: (callback: () => void | Promise<void>, delayMs: number) => void;
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning") => void;
    readonly onTerminalInput: (handler: () => void) => () => void;
    readonly getEditorText: () => string;
  };
};
type Handler = (event: unknown, context: Context) => Promise<unknown>;
const originalFetch = globalThis.fetch;
const originalEnvironment = {
  ENVOY_NATS_URL: process.env.ENVOY_NATS_URL,
  ENVOY_URL: process.env.ENVOY_URL,
  LEGION_BOOT_TOKEN: process.env.LEGION_BOOT_TOKEN,
  LEGION_DAEMON_URL: process.env.LEGION_DAEMON_URL,
  LEGION_GENERATION: process.env.LEGION_GENERATION,
  LEGION_ISSUE: process.env.LEGION_ISSUE,
  LEGION_ROLE: process.env.LEGION_ROLE,
  LEGION_TREE: process.env.LEGION_TREE,
  LEGION_STATE_DIR: process.env.LEGION_STATE_DIR,
  HOME: process.env.HOME,
  DISPATCH_URL: process.env.DISPATCH_URL,
  DISPATCH_TOKEN: process.env.DISPATCH_TOKEN,
  DISPATCH_STATE_DIR: process.env.DISPATCH_STATE_DIR,
} as const;
// No DISPATCH_* override reaches the panes here, beside the file above. The `dispatch` command's
// state (the session title file legion.ts writes) goes to a scratch directory.
let dispatchState = "";
beforeEach(() => {
  process.env.HOME = "/nonexistent-home-for-legion-tests";
  delete process.env.DISPATCH_URL;
  delete process.env.DISPATCH_TOKEN;
  dispatchState = mkdtempSync(join(tmpdir(), "legion-role-claim-dispatch-"));
  process.env.DISPATCH_STATE_DIR = dispatchState;
});
afterEach(() => {
  rmSync(dispatchState, { recursive: true, force: true });
  globalThis.fetch = originalFetch;
  for (const [key, value] of Object.entries(originalEnvironment)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
});
test("keeps a Legion role claimant fresh regardless of extension initialization order", async () => {
  const handlers = new Map<string, Handler[]>();
  const intervals: (() => void)[] = [];
  // The extra heartbeat call a failed pass arms on the host's managed timer, as the host would
  // hold it: recorded rather than run, so a test runs it when it means to.
  const retries: { readonly callback: () => void | Promise<void>; readonly delayMs: number }[] = [];
  // The warning the pane shows when a heartbeat pass fails, which also names the retry it armed.
  const heartbeatOutage = Promise.withResolvers<string>();
  const registrations: { readonly session_id: string; readonly topics: readonly string[] }[] = [];
  const heartbeatRegistration = Promise.withResolvers<void>();
  const tree = "REPO-42";
  const sessionID = "ses_legion_root";
  const role = roleToken("omp", tree, "architect");
  process.env.ENVOY_NATS_URL = "nats://nats-under-test:4222";
  process.env.ENVOY_URL = "http://envoy.test";
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  process.env.LEGION_BOOT_TOKEN = "claim-heartbeat";
  process.env.LEGION_STATE_DIR = "/tmp/legion-state";
  process.env.LEGION_TREE = tree;
  process.env.LEGION_ROLE = "architect";
  process.env.LEGION_ISSUE = tree;
  globalThis.fetch = (async (input, init) => {
    const url = new URL(input.toString());
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken: role,
        tree,
        issue: tree,
        role: "architect",
        generation: 3,
        secret: "root-secret",
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/v1/interests/subscribe") {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as {
        readonly session_id: string;
        readonly topics: readonly string[];
      };
      registrations.push(body);
      if (registrations.length === 3) heartbeatRegistration.resolve();
      return Response.json({
        session_id: body.session_id,
        machine_id: "test",
        dir: "/tmp",
        topics: body.topics,
      });
    }
    return Response.json({ session_id: sessionID, machine_id: "test", dir: "/tmp", topics: [] });
  }) as typeof fetch;
  const activeTools = ["read", "task", "wait"];
  const property = (): ZodNumberProperty => ({
    optional: property,
    nullable: property,
    describe: property,
    int: property,
    min: property,
    max: property,
  });
  const createPi = () => ({
    zod: {
      object: (shape: unknown) => shape,
      string: property,
      number: property,
      boolean: property,
      array: property,
      enum: property,
      unknown: property,
      discriminatedUnion: () => ({}),
    },
    sendMessage: () => undefined,
    appendEntry: () => undefined,
    setSessionName: async () => undefined,
    getActiveTools: () => activeTools,
    setActiveTools: async (tools: string[]) => {
      activeTools.splice(0, activeTools.length, ...tools);
    },
    on: (event: string, handler: Handler) => {
      const eventHandlers = handlers.get(event);
      if (eventHandlers === undefined) handlers.set(event, [handler]);
      else eventHandlers.push(handler);
    },
    registerTool: () => undefined,
    registerCommand: () => undefined,
    registerMessageRenderer: () => undefined,
  });
  const envoyPi = createPi();
  const legionPi = createPi();
  const context: Context = {
    cwd: "/tmp/legion-workspace",
    sessionManager: {
      getSessionId: () => sessionID,
      getSessionFile: () => "/tmp/legion-root.jsonl",
      ensureOnDisk: async () => undefined,
      getEntries: () => [],
    },
    setInterval: (callback) => intervals.push(callback),
    setTimeout: (callback, delayMs) => retries.push({ callback, delayMs }),
    ui: {
      notify: (message) => {
        if (message.includes("registry heartbeat failed")) heartbeatOutage.resolve(message);
      },
      onTerminalInput: () => () => undefined,
      getEditorText: () => "",
    },
  };

  // Legion initializes before Envoy — the opposite of load order in the
  // manifest — to prove claimEnvoyRole's bridge waits rather than binding to
  // a claim function that was never set.
  legionExtension(legionPi as never);
  envoyExtension(envoyPi as never);
  const sessionStart = handlers.get("session_start")?.[0];
  if (sessionStart === undefined) {
    throw new Error("Legion lifecycle handlers were not registered");
  }
  await sessionStart({}, context);

  // Two managed intervals: the role claimant's own heartbeat, armed first by the claim
  // bootstrap, and the session activity report every claimed session sends on change
  // (INFRA-945). The heartbeat is the one this test drives.
  expect(intervals).toHaveLength(2);
  // Establishing the role claimant registers before the role is applied, then
  // refreshes it after the claim.
  expect(registrations).toHaveLength(2);
  intervals[0]?.();
  await heartbeatRegistration.promise;
  expect(registrations).toHaveLength(3);
  expect(registrations[2]).toMatchObject({
    session_id: sessionID,
    topics: [agentSubject(sessionID)],
  });
  // The heartbeat pass reads the role back, which this listener stub answers with no record, so
  // the pass fails: the pane arms the one extra call a failed heartbeat buys on the host's
  // managed timer, at the shared draw, and says so — rather than throwing for want of one.
  expect(await heartbeatOutage.promise).toContain("retrying once in");
  expect(retries).toHaveLength(1);
  expect(retries[0]?.delayMs).toBeGreaterThanOrEqual(HEARTBEAT_RETRY_MIN_MS);
  expect(retries[0]?.delayMs).toBeLessThanOrEqual(HEARTBEAT_RETRY_MAX_MS);
  // What that call then does is pi-envoy's to pin (`envoy.test.ts`, "a registration that times
  // out is retried once inside forty seconds"); here it is enough that the pane armed it on the
  // host rather than throwing inside the heartbeat.
});
