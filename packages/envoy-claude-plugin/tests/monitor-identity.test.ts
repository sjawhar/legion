import { expect, test } from "bun:test"

test("prefers an explicitly configured Envoy session ID", async () => {
  // given
  const module = await import("../src/monitor-identity")
  const candidate = Reflect.get(module, "monitorSessionId")

  // when
  const output =
    typeof candidate === "function"
      ? candidate({ ENVOY_SESSION_ID: "claude-adapter", CLAUDE_SESSION_ID: "claude-native" }, 0)
      : undefined

  // then
  expect(output).toBe("claude-adapter")
})

test("uses Claude's native session ID when no Envoy override exists", async () => {
  // given
  const module = await import("../src/monitor-identity")
  const candidate = Reflect.get(module, "monitorSessionId")

  // when
  const output =
    typeof candidate === "function" ? candidate({ CLAUDE_SESSION_ID: "claude-native" }, 0) : undefined

  // then
  expect(output).toBe("claude-native")
})

test("derives a stable local identity from Claude's parent process", async () => {
  // given
  const module = await import("../src/monitor-identity")
  const candidate = Reflect.get(module, "monitorSessionId")

  // when
  const output = typeof candidate === "function" ? candidate({}, 4312) : undefined

  // then
  expect(output).toBe("claude-4312")
})
