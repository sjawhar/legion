/**
 * Test double for the host's `AgentRegistry` (`@oh-my-pi/pi-coding-agent`), shared by every
 * plugin's suites. `bun test` runs the suites in one process, and a module evaluated
 * under one suite's `mock.module` keeps that binding for the rest of the run, so every suite's
 * mock of the host package spreads this in and the roster itself lives on `globalThis`: whichever
 * mock won, the subagent check (`registeredSubagent`) reads the roster the running test filled.
 */

/** A file command or prompt template as the host's `AgentSession` getters list it. */
export interface TestSessionCommand {
  readonly name: string;
  readonly description?: string;
}

export interface TestAgentRef {
  readonly id: string;
  readonly kind: "main" | "sub" | "advisor";
  readonly session: {
    readonly sessionManager: { getSessionId(): string };
    /** The live session's `slashCommands` and `promptTemplates`, which pi-envoy's commands
     *  frame lists. */
    readonly slashCommands?: readonly TestSessionCommand[];
    readonly promptTemplates?: readonly TestSessionCommand[];
  } | null;
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
