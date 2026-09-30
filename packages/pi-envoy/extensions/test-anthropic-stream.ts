// The Anthropic Messages stream a stand-in model gateway answers with, shared by the tests that run
// the real Oh My Pi binary against one (legion-phase-stall-omp.test.ts, dispatch-first-omp.test.ts).

export type Block =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "tool_use"; readonly name: string; readonly input: Record<string, unknown> };

/** The Anthropic Messages stream for one scripted reply. */
export function messageStream(blocks: readonly Block[], id: string): string {
  const events: [string, unknown][] = [
    [
      "message_start",
      {
        type: "message_start",
        message: {
          id,
          type: "message",
          role: "assistant",
          model: "standin-model",
          content: [],
          stop_reason: null,
          stop_sequence: null,
          usage: { input_tokens: 5, output_tokens: 1 },
        },
      },
    ],
  ];
  blocks.forEach((block, index) => {
    if (block.type === "text") {
      events.push([
        "content_block_start",
        { type: "content_block_start", index, content_block: { type: "text", text: "" } },
      ]);
      events.push([
        "content_block_delta",
        { type: "content_block_delta", index, delta: { type: "text_delta", text: block.text } },
      ]);
    } else {
      events.push([
        "content_block_start",
        {
          type: "content_block_start",
          index,
          content_block: {
            type: "tool_use",
            id: `toolu_${id}_${index}`,
            name: block.name,
            input: {},
          },
        },
      ]);
      events.push([
        "content_block_delta",
        {
          type: "content_block_delta",
          index,
          delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input) },
        },
      ]);
    }
    events.push(["content_block_stop", { type: "content_block_stop", index }]);
  });
  const stopReason = blocks.some((block) => block.type === "tool_use") ? "tool_use" : "end_turn";
  events.push([
    "message_delta",
    {
      type: "message_delta",
      delta: { stop_reason: stopReason, stop_sequence: null },
      usage: { output_tokens: 1 },
    },
  ]);
  events.push(["message_stop", { type: "message_stop" }]);
  return events.map(([name, data]) => `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`).join("");
}
