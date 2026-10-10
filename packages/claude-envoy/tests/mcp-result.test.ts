import { expect, test } from "bun:test"
import { mcpResult } from "../src/envoy-channel-server"

test("a tool result is one JSON text block", () => {
  expect(mcpResult({ session_id: "ses_claude", dir: "/work" })).toEqual({
    content: [{ type: "text", text: JSON.stringify({ session_id: "ses_claude", dir: "/work" }) }],
  })
})
