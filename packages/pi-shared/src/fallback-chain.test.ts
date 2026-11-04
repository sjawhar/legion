import { beforeEach, describe, expect, mock, test } from "bun:test";
import type { HostModel } from "./pi-types";

/**
 * The chain resolver against fakes of the three Oh My Pi modules it loads, registered before the
 * module under test is imported, exactly as the AskGate entry's test registers its own
 * (`packages/pi-envoy/extensions/askgate/askgate.test.ts:5118-5240`): a static import of the
 * resolver would bind the real specifiers, which resolve only inside Oh My Pi.
 */

/** The deployment's `retry.fallbackChains`, keyed as the host keys them. */
const CHAINS: Record<string, string[]> = {
  "anthropic/claude-opus-5-5": ["anthropic/claude-opus-4-8", "openai/gpt-5.5"],
  implementer: ["anthropic/claude-opus-5-5", "anthropic/claude-opus-4-8"],
  "anthropic/claude-opus-4-8": [],
};
const MODEL: HostModel = { provider: "anthropic", id: "claude-opus-5-5" };
const modelRoles: Record<string, string> = { implementer: "anthropic/claude-opus-5-5:high" };

let retry = { enabled: true, modelFallback: true };
/** Selectors `ctx.models.resolve` answers nothing for: a model this session cannot use. */
const unresolvable = new Set<string>();
/** Set when a host module itself fails, as one does when the chain settings are unreadable. */
let chainsThrow = false;
/** The role hint each resolution was keyed by. */
const roleHints: Array<string | undefined> = [];

mock.module("@oh-my-pi/pi-coding-agent/config/settings", () => ({
  settings: { getModelRole: (role: string) => modelRoles[role] },
}));
mock.module("@oh-my-pi/pi-coding-agent/session/settings", () => ({
  cfgRetry: { get: () => retry },
}));
mock.module("@oh-my-pi/pi-coding-agent/session/retry-fallback-chains", () => ({
  getRetryFallbackChains: () => {
    if (chainsThrow) throw new Error("retry.fallbackChains is not readable");
    return CHAINS;
  },
  resolveRetryFallbackChainKey: (
    _context: unknown,
    selector: string,
    _model: unknown,
    roleHint?: string
  ) => {
    roleHints.push(roleHint);
    if (roleHint !== undefined && CHAINS[roleHint] !== undefined) return roleHint;
    return CHAINS[selector] === undefined ? undefined : selector;
  },
  findRetryFallbackCandidates: (_context: unknown, chainKey: string, selector: string) => {
    const chain = CHAINS[chainKey] ?? [];
    const after = chain.indexOf(selector) + 1;
    return chain.slice(after).map((raw) => {
      const [provider, id] = raw.split("/");
      return { raw, provider, id };
    });
  },
}));

const warnings: Array<{ message: string; context?: Record<string, unknown> }> = [];
const ctx = {
  modelRegistry: { find: () => undefined, hasProvider: () => true },
  models: {
    resolve: (spec: string): HostModel | undefined => {
      if (unresolvable.has(spec)) return undefined;
      const [provider, id] = spec.split("/");
      return provider === undefined || id === undefined ? undefined : { provider, id };
    },
  },
  log: {
    warn: (message: string, context?: Record<string, unknown>) =>
      void warnings.push({ message, context }),
  },
};

beforeEach(() => {
  retry = { enabled: true, modelFallback: true };
  unresolvable.clear();
  chainsThrow = false;
  roleHints.length = 0;
  warnings.length = 0;
});

/** Imported after the fakes are registered, never at the head of the file. */
async function nextModel(model: HostModel, roleHint?: string) {
  const { nextFallbackModel } = await import("./fallback-chain");
  return nextFallbackModel(ctx, model, roleHint);
}

describe("the model below this one", () => {
  test("a chain names the next model", async () => {
    expect(await nextModel(MODEL)).toEqual({ provider: "anthropic", id: "claude-opus-4-8" });
    expect(roleHints).toEqual([undefined]);
  });

  test("the role the session runs under keys the chain", async () => {
    expect(await nextModel(MODEL, "implementer")).toEqual({
      provider: "anthropic",
      id: "claude-opus-4-8",
    });
    expect(roleHints).toEqual(["implementer"]);
  });

  test("a model the chain ends on has nothing below it", async () => {
    expect(await nextModel({ provider: "anthropic", id: "claude-opus-4-8" })).toBeUndefined();
  });

  test("a model no chain covers has nothing below it", async () => {
    expect(await nextModel({ provider: "google", id: "gemini-3-pro" })).toBeUndefined();
  });

  test("a next model this session cannot use is no fallback", async () => {
    unresolvable.add("anthropic/claude-opus-4-8");
    expect(await nextModel(MODEL)).toBeUndefined();
  });
});

describe("a deployment that wants no fallback", () => {
  test("retry.enabled false answers nothing", async () => {
    retry = { enabled: false, modelFallback: true };
    expect(await nextModel(MODEL)).toBeUndefined();
  });

  test("retry.modelFallback false answers nothing", async () => {
    retry = { enabled: true, modelFallback: false };
    expect(await nextModel(MODEL)).toBeUndefined();
  });
});

describe("a host that fails", () => {
  test("a module that throws is one warning and no model", async () => {
    chainsThrow = true;
    expect(await nextModel(MODEL)).toBeUndefined();
    expect(warnings).toHaveLength(1);
    expect(warnings[0]?.message).toBe(
      "legion: the model fallback chain did not resolve; staying on this model"
    );
    expect(warnings[0]?.context).toEqual({
      model: "anthropic/claude-opus-5-5",
      error: "retry.fallbackChains is not readable",
    });
  });
});
