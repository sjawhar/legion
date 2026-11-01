import { expect, test } from "bun:test"
import { claudeProjectDirectory, claudeSessionId } from "../src/claude-session"

test("prefers an explicit Envoy session ID for controlled channel QA", () => {
  expect(
    claudeSessionId({ ENVOY_SESSION_ID: "claude-qa", CLAUDE_CODE_SESSION_ID: "claude-native" }),
  ).toBe("claude-qa")
})

test("uses the project directory rather than the plugin working directory", () => {
  expect(claudeProjectDirectory({ CLAUDE_PROJECT_DIR: "/work/project" }, "/plugin/root")).toBe(
    "/work/project",
  )
})

test("rejects a missing session identity instead of inventing one", () => {
  expect(() => claudeSessionId({})).toThrow(
    "Envoy channel requires ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID; Claude Code did not provide a session identity",
  )
})
