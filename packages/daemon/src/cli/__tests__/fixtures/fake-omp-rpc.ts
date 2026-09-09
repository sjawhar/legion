#!/usr/bin/env bun
/**
 * Stands in for `omp --mode rpc` in worker-shim tests: negotiates protocol v2, acks a prompt
 * immediately then streams agent_start/agent_end, and exits 0 on stdin EOF (mirrors the real
 * rpc-mode's stdin-close shutdown contract).
 */
const decoder = new TextDecoder();
let buffer = "";

export {};

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
      console.log(
        JSON.stringify({ id: command.id, type: "response", command: "prompt", success: true })
      );
      console.log(JSON.stringify({ type: "agent_start" }));
      console.log(
        JSON.stringify({
          type: "agent_end",
          messages: [{ role: "user", content: command.message }],
        })
      );
    } else if (command.type === "get_state") {
      console.log(
        JSON.stringify({
          id: command.id,
          type: "response",
          command: "get_state",
          success: true,
          data: {},
        })
      );
    }
  }
}
process.exit(0);
