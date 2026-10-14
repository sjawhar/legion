import { expect, test } from "bun:test"
import hooks from "../hooks/hooks.json" with { type: "json" }

// Each hook resolves Dispatch before it writes anything, and a `!` dispatch.token runs for up to
// 10 s (envoy-client's COMMAND_TIMEOUT_MS) in that hook's own process, after Bun has started. Claude
// Code's `timeout` is in seconds; a hook it kills first loses the open-asks summary or the
// dispatch-first context silently, so every hook outlasts the command with room for start-up.
test("every hook's timeout is 20 s, above the 10 s ! command timeout plus start-up", () => {
  const timeouts = Object.values(hooks.hooks).flatMap((matchers) =>
    matchers.flatMap((matcher) => matcher.hooks.map((hook) => [hook.args[1], hook.timeout])),
  )
  expect(timeouts).toEqual([
    ["open-asks", 20],
    ["dispatch-first", 20],
    ["dispatch-first", 20],
  ])
})
