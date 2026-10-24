import { expect, test } from "bun:test"
import hooks from "../hooks/hooks.json" with { type: "json" }

// Each SessionStart hook resolves Dispatch before it writes anything, and a `!` dispatch.token runs
// for up to 10 s (envoy-client's COMMAND_TIMEOUT_MS) in that hook's own process, after Bun has
// started. Claude Code's `timeout` is in seconds; a hook it kills first loses the open-asks summary
// or the dispatch-first context silently, so each of those outlasts the command with room for
// start-up. The session-title hook resolves nothing and makes no network call while Claude Code
// blocks the prompt on it, so it gets a far shorter bound.
test("each hook's timeout matches the work it does: 20 s for the Dispatch hooks, 5 s for session-title", () => {
  const timeouts = Object.values(hooks.hooks).flatMap((matchers) =>
    matchers.flatMap((matcher) => matcher.hooks.map((hook) => [hook.args[1], hook.timeout])),
  )
  expect(timeouts).toEqual([
    ["open-asks", 20],
    ["dispatch-first", 20],
    ["dispatch-first", 20],
    ["session-title", 5],
  ])
})
