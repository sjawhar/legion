import { isRecord, resolveSessionID } from "./utils";

const DEFAULT_THRESHOLD = 5;
const DEFAULT_ACTION = "abort" as const;

export interface CircuitBreakerConfig {
  enabled?: boolean;
  threshold?: number;
  action?: "warn" | "abort";
}

/**
 * Recursively sorts object keys to produce a stable JSON string
 * regardless of key insertion order.
 * e.g. {b:1,a:2} and {a:2,b:1} both produce '{"a":2,"b":1}'
 */
function sortedStringify(value: unknown): string {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return JSON.stringify(value);
  }
  const record = value as Record<string, unknown>;
  const sorted: Record<string, unknown> = {};
  for (const key of Object.keys(record).sort()) {
    sorted[key] = record[key];
  }
  return JSON.stringify(sorted, (_key, v: unknown) => {
    if (v !== null && typeof v === "object" && !Array.isArray(v)) {
      const r = v as Record<string, unknown>;
      const s: Record<string, unknown> = {};
      for (const k of Object.keys(r).sort()) {
        s[k] = r[k];
      }
      return s;
    }
    return v;
  });
}

/**
 * Circuit breaker hook that detects repetitive identical tool calls per session.
 *
 * Tracking structure: Map<sessionID, Map<"toolName:argsHash", callCount>>
 *
 * Triggers when count reaches threshold (default 5).
 * action "abort": throws Error (rejects the tool call, does not abort session).
 * action "warn": logs to console.warn only.
 * Cleanup: clears session tracking on session.deleted event.
 */
export function createCircuitBreakerHook(config: CircuitBreakerConfig = {}) {
  const enabled = config.enabled !== false;
  const threshold = config.threshold ?? DEFAULT_THRESHOLD;
  const action = config.action ?? DEFAULT_ACTION;

  // Map<sessionID, Map<"toolName:argsHash", count>>
  const sessionCounts = new Map<string, Map<string, number>>();

  // Signature matches tool.execute.before: input has tool/sessionID/callID,
  // output has args (the parsed tool input).
  // See subagentQuestionBlockerHook for the same pattern.
  const toolExecuteBefore = (
    input: { tool: string; sessionID?: string; callID?: string; args?: unknown },
    output: { args?: unknown }
  ): void => {
    if (!enabled) return;

    const sessionID = typeof input.sessionID === "string" ? input.sessionID : undefined;
    if (!sessionID) return;

    const toolName = typeof input.tool === "string" ? input.tool : "";
    // Args live in output.args (parsed tool input) in tool.execute.before
    const args = isRecord(output) && "args" in output ? output.args : input.args;
    const argsHash = sortedStringify(args ?? {});
    const key = `${toolName}:${argsHash}`;

    let sessionMap = sessionCounts.get(sessionID);
    if (!sessionMap) {
      sessionMap = new Map<string, number>();
      sessionCounts.set(sessionID, sessionMap);
    }

    const count = (sessionMap.get(key) ?? 0) + 1;
    sessionMap.set(key, count);

    if (count >= threshold) {
      const message =
        `[circuit-breaker] Repetitive tool use detected: "${toolName}" called ${count} times ` +
        `with identical arguments in session "${sessionID}". ` +
        `This may indicate a stuck loop. Vary your approach or use a different tool.`;

      if (action === "abort") {
        throw new Error(message);
      } else {
        console.warn(message);
      }
    }
  };

  const event = async ({
    event,
  }: {
    event: { type: string; properties?: unknown };
  }): Promise<void> => {
    if (event.type === "session.deleted") {
      const props = isRecord(event.properties) ? event.properties : undefined;
      const sessionID = resolveSessionID(props);
      if (sessionID) {
        sessionCounts.delete(sessionID);
      }
    }
  };

  return {
    "tool.execute.before": toolExecuteBefore,
    event,
  };
}
