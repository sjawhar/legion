#!/usr/bin/env bun
/**
 * Stands in for `omp --mode rpc` whose prompt acknowledgement is not followed by a turn or is
 * followed only late. Negotiates protocol v2, appends each prompt's message as one line to
 * `FIXTURE_PROMPT_LOG`, and can independently delay the acknowledgement with
 * `FIXTURE_PROMPT_RESPONSE_DELAY_MS` and the accepted delivery's `agent_start` with
 * `FIXTURE_AGENT_START_DELAY_MS`. `FIXTURE_FIRST_NEGOTIATE_RESPONSE_DELAY_MS` delays only the
 * first protocol response. `FIXTURE_FOREIGN_TURN_DELAY_MS` emits one unrelated turn after the
 * prompt is received, and `FIXTURE_INITIAL_FOREIGN_TURN_DELAY_MS` does so before a prompt. Both
 * let the real shim test model a turn unrelated to the pending delivery. `agent_end` follows each
 * start by 200 ms; an unset delivery start delay emits no delivery turn (the fork build that
 * accepts a message and starts no turn). `get_state` answers `{ data: { isStreaming: <a turn is
 * running> } }`; stdin EOF exits 0.
 */
import { appendFileSync } from "node:fs";

const decoder = new TextDecoder();
let buffer = "";
let streaming = false;
const promptLog = process.env.FIXTURE_PROMPT_LOG;
const eventLog = process.env.FIXTURE_EVENT_LOG;
/** Reads an optional whole-millisecond delay from the environment; unset means "never". */
const readDelayMs = (name: string): number | undefined => {
  const raw = process.env[name];
  if (raw === undefined) return undefined;
  const ms = Number(raw);
  if (!Number.isSafeInteger(ms)) {
    throw new Error(`${name} must be a whole number of ms: ${raw}`);
  }
  return ms;
};
const startDelayMs = readDelayMs("FIXTURE_AGENT_START_DELAY_MS");
const responseDelayMs = readDelayMs("FIXTURE_PROMPT_RESPONSE_DELAY_MS");
const foreignTurnDelayMs = readDelayMs("FIXTURE_FOREIGN_TURN_DELAY_MS");
const initialForeignTurnDelayMs = readDelayMs("FIXTURE_INITIAL_FOREIGN_TURN_DELAY_MS");
const firstNegotiateDelayMs = readDelayMs("FIXTURE_FIRST_NEGOTIATE_RESPONSE_DELAY_MS");

const recordEvent = (event: string): void => {
  if (eventLog) appendFileSync(eventLog, `${event}\n`);
};

const emitTurn = (kind: "delivery" | "foreign", message: string | undefined): void => {
  streaming = true;
  recordEvent(`${kind}:start`);
  console.log(JSON.stringify({ type: "agent_start" }));
  setTimeout(() => {
    streaming = false;
    recordEvent(`${kind}:end`);
    console.log(
      JSON.stringify({
        type: "agent_end",
        messages: [{ role: "user", content: message }],
      })
    );
  }, 200);
};
if (initialForeignTurnDelayMs !== undefined) {
  setTimeout(() => emitTurn("foreign", "unrelated turn"), initialForeignTurnDelayMs);
}
let negotiations = 0;

for await (const chunk of Bun.stdin.stream()) {
  buffer += decoder.decode(chunk, { stream: true });
  let index = buffer.indexOf("\n");
  while (index !== -1) {
    const line = buffer.slice(0, index).trim();
    buffer = buffer.slice(index + 1);
    index = buffer.indexOf("\n");
    if (!line) continue;
    const command = JSON.parse(line) as { id?: string; type: string; message?: string };
    if (command.type === "negotiate_protocol") {
      negotiations += 1;
      const negotiate = () => {
        console.log(
          JSON.stringify({
            id: command.id,
            type: "response",
            command: "negotiate_protocol",
            success: true,
            data: { protocolVersion: 2 },
          })
        );
      };
      if (negotiations === 1 && firstNegotiateDelayMs !== undefined) {
        setTimeout(negotiate, firstNegotiateDelayMs);
      } else {
        negotiate();
      }
    } else if (command.type === "prompt") {
      if (promptLog) appendFileSync(promptLog, `${command.message ?? ""}\n`);
      if (streaming) {
        recordEvent("prompt:refused");
        console.log(
          JSON.stringify({
            id: command.id,
            type: "response",
            command: "prompt",
            success: false,
            error: "AgentBusyError",
          })
        );
        continue;
      }
      const acknowledge = () => {
        recordEvent("prompt:ack");
        console.log(
          JSON.stringify({ id: command.id, type: "response", command: "prompt", success: true })
        );
      };
      if (responseDelayMs === undefined) acknowledge();
      else setTimeout(acknowledge, responseDelayMs);
      if (foreignTurnDelayMs !== undefined) {
        setTimeout(() => emitTurn("foreign", "unrelated turn"), foreignTurnDelayMs);
      }
      if (startDelayMs === 0) {
        emitTurn("delivery", command.message);
      } else if (startDelayMs !== undefined) {
        setTimeout(() => emitTurn("delivery", command.message), startDelayMs);
      }
    } else if (command.type === "get_state") {
      console.log(
        JSON.stringify({
          id: command.id,
          type: "response",
          command: "get_state",
          success: true,
          data: { isStreaming: streaming },
        })
      );
    }
  }
}
process.exit(0);
