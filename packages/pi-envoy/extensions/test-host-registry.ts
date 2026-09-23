/**
 * Test double for the host's `AgentRegistry` (`@oh-my-pi/pi-coding-agent`), shared by every
 * suite in this directory. `bun test` runs the suites in one process, and a module evaluated
 * under one suite's `mock.module` keeps that binding for the rest of the run, so every suite's
 * mock of the host package spreads this in and the roster itself lives on `globalThis`: whichever
 * mock won, `isRegisteredSubagent` reads the roster the running test filled.
 */

export interface TestAgentRef {
  readonly id: string;
  readonly kind: "main" | "sub" | "advisor";
  readonly session: { readonly sessionManager: { getSessionId(): string } } | null;
  readonly sessionFile: string | null;
}

const ROSTER = Symbol.for("legion.pi-envoy.test-agent-roster");

interface GlobalRoster {
  [ROSTER]?: TestAgentRef[];
}

export function testAgentRoster(): TestAgentRef[] {
  const store = globalThis as unknown as GlobalRoster;
  store[ROSTER] ??= [];
  return store[ROSTER];
}

export const hostAgentRegistryMock = {
  AgentRegistry: { global: () => ({ list: () => testAgentRoster() }) },
};
