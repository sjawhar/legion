import { expect, test } from "bun:test"

test("parses a destination and a multiword message", async () => {
  // given
  const module = await import("../src/send-arguments")
  const candidate = Reflect.get(module, "parseSendArguments")

  // when
  const output =
    typeof candidate === "function" ? candidate(["ses_receiver", "ready", "for", "QA"]) : undefined

  // then
  expect(output).toEqual({ targetSession: "ses_receiver", message: "ready for QA" })
})
