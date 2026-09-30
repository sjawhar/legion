import { afterEach, expect, spyOn, test } from "bun:test";
import { existsSync } from "node:fs";
import { logger } from "../log";
import initPlugin from "../server";

type ConfigHook = (cfg: { instructions?: string[] } & Record<string, unknown>) => void;

const previous = { ...process.env };
afterEach(() => {
  process.env = { ...previous };
});

async function configure(): Promise<{ instructions?: string[] }> {
  const warn = spyOn(logger, "warn").mockImplementation(() => {});
  const hooks = await initPlugin({ serverUrl: new URL("http://127.0.0.1:9/") });
  try {
    const cfg: { instructions?: string[] } & Record<string, unknown> = {};
    (hooks.config as ConfigHook)(cfg);
    return cfg;
  } finally {
    hooks.dispose?.();
    warn.mockRestore();
  }
}

test("with Dispatch configured, every session gets the dispatch-first skill as an instruction file", async () => {
  process.env.DISPATCH_URL = "http://127.0.0.1:9";
  process.env.DISPATCH_TOKEN = "test-token";

  const { instructions } = await configure();

  expect(instructions).toHaveLength(1);
  const [skill] = instructions ?? [];
  expect(skill).toEndWith("/skills/dispatch-first/SKILL.md");
  expect(existsSync(skill ?? "")).toBe(true);
});

test("without Dispatch, sessions get no dispatch-first instruction", async () => {
  delete process.env.DISPATCH_URL;
  delete process.env.DISPATCH_TOKEN;
  process.env.HOME = "/nonexistent-home-for-envoy-plugin-test";
  delete process.env.DISPATCH_TOKEN_FILE;

  const { instructions } = await configure();

  expect(instructions).toBeUndefined();
});
