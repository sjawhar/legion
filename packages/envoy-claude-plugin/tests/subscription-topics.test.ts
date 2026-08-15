import { expect, test } from "bun:test"

test("includes the session route and configured topic patterns", async () => {
  // given
  const module = await import("../src/subscription-topics")
  const candidate = Reflect.get(module, "subscriptionTopics")

  // when
  const output =
    typeof candidate === "function"
      ? candidate({
          sessionId: "claude-qa",
          additionalTopics: "notifications.role.qa, team.bridge.>",
        })
      : undefined

  // then
  expect(output).toEqual([
    "notifications.agent.claude-qa",
    "notifications.role.qa",
    "team.bridge.>",
  ])
})
