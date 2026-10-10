import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { statSync } from "node:fs";
import path from "node:path";
import { logger } from "../log";
import initPlugin from "../server";

type ShellEnv = (
  input: { cwd: string; sessionID?: string; callID?: string },
  output: { env: Record<string, string> }
) => Promise<void>;

interface Hooks {
  readonly tool: Record<string, unknown>;
  readonly "shell.env"?: ShellEnv;
  event(input: { event: { type?: string; properties?: Record<string, unknown> } }): Promise<void>;
  dispose(): void;
}

const previous = { ...process.env };
const originalFetch = globalThis.fetch;
let dispose: (() => void) | undefined;
beforeEach(() => {
  process.env.DISPATCH_URL = "http://127.0.0.1:9";
  process.env.DISPATCH_TOKEN = "test-token";
  process.env.ENVOY_URL = "http://127.0.0.1:59999";
  spyOn(logger, "warn").mockImplementation(() => {});
});
afterEach(() => {
  dispose?.();
  dispose = undefined;
  globalThis.fetch = originalFetch;
  process.env = { ...previous };
});

async function load(): Promise<Hooks> {
  const hooks = (await initPlugin({ serverUrl: new URL("http://127.0.0.1:13381/") })) as Hooks;
  dispose = hooks.dispose;
  return hooks;
}

async function shellEnv(
  hooks: Hooks,
  input: { cwd: string; sessionID?: string },
  env: Record<string, string> = {}
): Promise<Record<string, string>> {
  const shell = hooks["shell.env"];
  expect(shell).toBeFunction();
  const output = { env: { ...env } };
  await shell?.(input, output);
  return output.env;
}

test("shell.env puts an executable dispatch first on PATH and names the OpenCode session", async () => {
  const hooks = await load();

  const env = await shellEnv(hooks, { cwd: "/tmp", sessionID: "ses_opencode" });

  const [first, ...rest] = (env.PATH ?? "").split(path.delimiter);
  expect(statSync(path.join(first ?? "", "dispatch")).mode & 0o111).toBe(0o111);
  expect(rest.join(path.delimiter)).toBe(process.env.PATH ?? "");
  expect(env.DISPATCH_HOST).toBe("opencode");
  expect(env.DISPATCH_SESSION_ID).toBe("ses_opencode");
});

test("shell.env carries the title the plugin tracks for the session", async () => {
  const titled = Promise.withResolvers<void>();
  globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (url.endsWith("/session/ses_titled")) {
      return Response.json({ id: "ses_titled", title: "Fix the flaky test" });
    }
    if (url.includes("/v1/interests/subscribe")) {
      // The plugin records the title it fetched, then re-registers the session with it.
      if (String(init?.body).includes('"title":"Fix the flaky test"')) titled.resolve();
      return Response.json({ session_id: "ses_titled", topics: [] });
    }
    throw new Error("connection refused");
  }) as typeof fetch;
  const hooks = await load();

  await hooks.event({
    event: {
      type: "session.status",
      properties: { sessionID: "ses_titled", status: { type: "busy" } },
    },
  });
  await titled.promise;
  const env = await shellEnv(hooks, { cwd: "/tmp", sessionID: "ses_titled" });

  expect(env.DISPATCH_SESSION_TITLE).toBe("Fix the flaky test");
});

// OpenCode runs the shell with `{...process.env, ...output.env}`, so a key the hook leaves out
// keeps whatever the OpenCode process inherited, such as a parent session's id.
test("shell.env without a session id empties the inherited session id and title", async () => {
  process.env.DISPATCH_SESSION_ID = "ses_parent";
  process.env.DISPATCH_SESSION_TITLE = "Parent session";
  const hooks = await load();

  const env = await shellEnv(hooks, { cwd: "/tmp" });
  const shell = { ...process.env, ...env };

  expect(env.DISPATCH_HOST).toBe("opencode");
  expect(shell.DISPATCH_SESSION_ID).toBe("");
  expect(shell.DISPATCH_SESSION_TITLE).toBe("");
});

test("an untitled session empties an inherited title", async () => {
  process.env.DISPATCH_SESSION_TITLE = "Parent session";
  const hooks = await load();

  const env = await shellEnv(hooks, { cwd: "/tmp", sessionID: "ses_untitled" });

  expect({ ...process.env, ...env }.DISPATCH_SESSION_TITLE).toBe("");
});

test("the plugin registers no dispatch_* tool, with Dispatch configured", async () => {
  const hooks = await load();

  expect(Object.keys(hooks.tool).filter((name) => name.startsWith("dispatch_"))).toEqual([]);
  expect(Object.keys(hooks.tool)).toContain("envoy_send");
});
