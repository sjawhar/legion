#!/usr/bin/env bun
/**
 * Stands in for `omp --mode rpc` in worker-shim tests: negotiates protocol v2, acks a prompt
 * immediately then streams agent_start/agent_end (as protocol v2 rpc_chunk lines when the prompt
 * starts with "chunked:"), and exits 0 on stdin EOF (mirrors the real rpc-mode's stdin-close
 * shutdown contract).
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
      const agentEnd = JSON.stringify({
        type: "agent_end",
        messages: [{ role: "user", content: command.message }],
      });
      if (command.message?.startsWith("chunked:")) {
        // A long turn's agent_end exceeds the 1 MiB frame limit and goes out as protocol v2
        // rpc_chunk lines: utf8 bytes of the logical frame in 256 KiB base64 slices.
        const padded = JSON.stringify({
          type: "agent_end",
          messages: [{ role: "user", content: command.message.padEnd(1_200_000, "x") }],
        });
        const bytes = Buffer.from(padded, "utf8");
        const payload = 256 * 1024;
        const count = Math.ceil(bytes.byteLength / payload);
        for (let index = 0; index < count; index += 1) {
          console.log(
            JSON.stringify({
              type: "rpc_chunk",
              chunkId: "agent-end-1",
              index,
              count,
              byteLength: bytes.byteLength,
              data: bytes.subarray(index * payload, (index + 1) * payload).toString("base64"),
            })
          );
        }
      } else {
        console.log(agentEnd);
      }
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
