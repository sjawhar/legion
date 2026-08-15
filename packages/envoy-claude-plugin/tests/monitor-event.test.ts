import { expect, test } from "bun:test"

test("formats a direct Envoy envelope as a Claude monitor event", async () => {
  // given
  const module = await import("../src/monitor-event")
  const candidate = Reflect.get(module, "formatMonitorEvent")

  // when
  const output =
    typeof candidate === "function"
      ? candidate(
          JSON.stringify({
            topic: "notifications.agent.claude-qa",
            source_session: "ses_sender",
            payload_summary: "wake and report ready",
          }),
        )
      : undefined

  // then
  expect(output).toBe(
    "[envoy topic=notifications.agent.claude-qa source=ses_sender] wake and report ready",
  )
})

test("keeps each inbound message on one monitor stdout line", async () => {
  // given
  const { formatMonitorEvent } = await import("../src/monitor-event")

  // when
  const output = formatMonitorEvent(
    JSON.stringify({
      topic: "notifications.agent.claude-qa",
      payload_summary: "first line\nsecond line",
    }),
  )

  // then
  expect(output).toBe(
    "[envoy topic=notifications.agent.claude-qa source=external] first line second line",
  )
})
