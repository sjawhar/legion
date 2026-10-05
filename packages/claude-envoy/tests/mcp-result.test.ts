import { expect, test } from "bun:test"
import { mcpResult } from "../src/envoy-channel-server"

test("a Dispatch result's pictures follow its JSON text as MCP image blocks", () => {
  expect(
    mcpResult({
      text: "Picture shot.png: image/png, version 1, 8 bytes.",
      details: { session: "s" },
      images: [{ data: "iVBORw0KGgo=", mimeType: "image/png" }],
    }),
  ).toEqual({
    content: [
      {
        type: "text",
        text: JSON.stringify({
          text: "Picture shot.png: image/png, version 1, 8 bytes.",
          details: { session: "s" },
        }),
      },
      { type: "image", data: "iVBORw0KGgo=", mimeType: "image/png" },
    ],
  })
})

test("a result without pictures stays one JSON text block", () => {
  expect(mcpResult({ text: "Posted.", details: {} })).toEqual({
    content: [{ type: "text", text: JSON.stringify({ text: "Posted.", details: {} }) }],
  })
})
