import { expect, test } from "bun:test"

import { monitorSessionId } from "../src/monitor-identity"

test("prefers an explicitly configured Envoy session ID", async () => {
  // given
  const environment = {
    ENVOY_SESSION_ID: "claude-adapter",
    CLAUDE_CODE_SESSION_ID: "claude-native",
  }

  // when
  const output = monitorSessionId(environment)

  // then
  expect(output).toBe("claude-adapter")
})

test("uses Claude Code's session ID when no Envoy override exists", async () => {
  // given
  const environment = { CLAUDE_CODE_SESSION_ID: "claude-native" }

  // when
  const output = monitorSessionId(environment)

  // then
  expect(output).toBe("claude-native")
})

test("rejects an absent session ID instead of inventing a PID-derived identity", async () => {
  // given
  // then
  expect(() => monitorSessionId({})).toThrow(
    "Envoy monitor requires ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID; Claude Code did not provide a session identity",
  )
})
