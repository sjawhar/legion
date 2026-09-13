#!/usr/bin/env bun
/**
 * Stands in for `omp --mode rpc` whose prompt acknowledgement is not followed by a turn — or is
 * followed only late. Negotiates protocol v2; acks every `prompt` immediately (exactly as the
 * real rpc mode does, before any turn starts) and appends the prompt's message as one line to
 * the file named by `FIXTURE_PROMPT_LOG`; emits `agent_start` `FIXTURE_AGENT_START_DELAY_MS`
 * milliseconds after the acknowledgement and `agent_end` 200 ms after that — or, when that
 * variable is unset, never emits either (the fork build that accepts a message and starts no
 * turn); answers `get_state` with `{ data: { isStreaming: <a turn is running> } }`; exits 0 on
 * stdin EOF (the real rpc mode's stdin-close shutdown contract).
 */
import { appendFileSync } from "node:fs";

const decoder = new TextDecoder();
let buffer = "";
let streaming = false;
const promptLog = process.env.FIXTURE_PROMPT_LOG;
const startDelay = process.env.FIXTURE_AGENT_START_DELAY_MS;
const startDelayMs = startDelay === undefined ? undefined : Number(startDelay);
if (startDelayMs !== undefined && !Number.isSafeInteger(startDelayMs)) {
  throw new Error(`FIXTURE_AGENT_START_DELAY_MS must be a whole number of ms: ${startDelay}`);
}

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
      console.log(
        JSON.stringify({
          id: command.id,
          type: "response",
          command: "negotiate_protocol",
          success: true,
          data: { protocolVersion: 2 },
        })
      );
    } else if (command.type === "prompt") {
      if (promptLog) appendFileSync(promptLog, `${command.message ?? ""}\n`);
      console.log(
        JSON.stringify({ id: command.id, type: "response", command: "prompt", success: true })
      );
      if (startDelayMs !== undefined) {
        setTimeout(() => {
          streaming = true;
          console.log(JSON.stringify({ type: "agent_start" }));
          setTimeout(() => {
            streaming = false;
            console.log(
              JSON.stringify({
                type: "agent_end",
                messages: [{ role: "user", content: command.message }],
              })
            );
          }, 200);
        }, startDelayMs);
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
