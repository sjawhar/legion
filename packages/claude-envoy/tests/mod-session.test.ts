/**
 * What this server does with a card in a Claude Code session (`ModSession.show`, INFRA-1026):
 * an Envoy delivery that is not the person's own message — an ask answer, a teammate's or an
 * architect's message, an issue event — is handed to the mod as one message, and the row the mod
 * answers with is the proof the session has it.
 *
 * Where that message goes, and what points the model at a row no request read, is the mod's own
 * and is driven there (`./mod.test.ts`): the mod is the one process where the turn's events and
 * this plugin's writes arrive in one order. A person's own message is driven through the router
 * that accepts it (`./user-turn-router.test.ts`).
 */
import { expect, test } from "bun:test"
import type { ModCallArguments, ModCallName, ModCallResults } from "../hooks/mod-protocol"
import { modSession } from "../src/mod-session"
import { settled } from "./channel-session-harness"

/** A card as the server hands one over: the rendered delivery's text, and the blocks it is. */
const CARD = "Dispatch message from alice: the migration is approved."
const BLOCKS = [{ text: CARD, type: "text" }]
const MESSAGE = "33333333-3333-4333-8333-333333333333"
const EVENT = "evt-42"

type Answer = (
  args: ModCallArguments["session.message"],
) => ModCallResults["session.message"] | Promise<ModCallResults["session.message"]>

/** A session on a mod that answers what the test says and remembers what it was asked. Its bound
 *  for the mod's answer is a tenth of a second, short enough that the test which drives a mod
 *  that answers nothing past it costs the suite that much and no more. */
function driven(answer?: Answer, showMs = 100) {
  const written: ModCallArguments["session.message"][] = []
  const session = modSession({
    link: {
      connected: () => true,
      request<N extends ModCallName>(
        call: N,
        args: ModCallArguments[N],
        options?: { readonly timeoutMs?: number },
      ): Promise<ModCallResults[N]> {
        if (call !== "session.message") {
          return Promise.reject(new Error(`the card path made an unexpected mod call: ${call}`))
        }
        const message = args as ModCallArguments["session.message"]
        written.push(message)
        if (answer !== undefined) {
          return Promise.resolve(answer(message)) as Promise<ModCallResults[N]>
        }
        // A mod that answers nothing, as the real link leaves one: the caller's own bound is
        // what ends the wait, and the wording is the link's (`../src/mod-link.ts`, `request`).
        const bound = options?.timeoutMs ?? 0
        const settle = Promise.withResolvers<ModCallResults[N]>()
        const timer = setTimeout(
          () => settle.reject(new Error(`the mod did not answer ${call} within ${bound} ms`)),
          bound,
        )
        timer.unref()
        return settle.promise
      },
    },
    showMs,
    warn: () => undefined,
  })
  return { session, written }
}

// The card is one message to the mod, carrying its Envoy event id: that id is what the mod's
// write is idempotent by, so the copy Envoy sends after an unanswered frame is written once
// (`../hooks/mod.ts`, `writeOnce`). What the card becomes in the session is the mod's to decide.
test("a card is handed to the mod as one message, under its own Envoy event id", async () => {
  const mod = driven(() => ({ as: "prompt", uuid: "prompt-row-1" }))

  await mod.session.show({ content: BLOCKS, eventId: EVENT, text: CARD })

  expect(mod.written).toEqual([
    { content: BLOCKS, eventId: EVENT, message: { kind: "card", text: CARD } },
  ])
})

// Repro `R-A` (INFRA-1026 review, round eleven): a request handed to a poll is a write into a
// socket buffer, which a SIGSTOPped Claude Code never reads, so the hand-out proved nothing
// about the session and a card it counted as entered was lost at the next SIGKILL. The row the
// mod answers with is the proof, and a mod that answers nothing leaves the frame unanswered —
// Envoy sends the card again, and the mod writes the one message once.
test("a card the mod never answers rejects, so its frame goes unanswered", async () => {
  const stopped = driven()

  await expect(
    stopped.session.show({ content: BLOCKS, eventId: EVENT, text: CARD }),
  ).rejects.toThrow("the mod did not answer session.message within 100 ms")
  expect(stopped.written).toHaveLength(1)
})

test("a card the session refused rejects with what it said", async () => {
  const refusing = driven(() => ({ as: "prompt", deny: "the conversation is compacting" }))

  await expect(
    refusing.session.show({ content: BLOCKS, eventId: EVENT, text: CARD }),
  ).rejects.toThrow("the session refused the card: the conversation is compacting")
})

// A notice this server writes for itself is no Envoy frame: it carries no event id, and the mod
// writes it as it comes.
test("a card with no Envoy event of its own is handed over without one", async () => {
  const mod = driven(() => ({ as: "row", uuid: "row-1" }))

  await mod.session.show({ content: BLOCKS, text: CARD })

  expect(mod.written[0]?.eventId).toBeUndefined()
})

// Which rows of the conversation are this session's own is a question the live view asks by
// uuid (`../src/agent-stream-claude`): it draws nothing of the row the prompt at unread rows
// wrote, since that prompt quotes rows it has already drawn, and it draws the one `isMeta` row
// a message became. Both are answered from the stamp the mod puts on the row's own event, never
// from what the row says: read off the wording, a card carrying that prompt's words was drawn
// for nobody, and read off the origin, another mod's appended context was drawn as the person's
// (INFRA-1026 review, round ten).
test("the rows the mod stamped are claimed by their uuids", () => {
  const mod = driven(() => ({ as: "row", uuid: "row-card" }))

  mod.session.observe({
    door: "note",
    kind: "append",
    message: { content: BLOCKS, role: "user", type: "user" },
    origin: { kind: "plugin", name: "claude-envoy" },
    uuid: "row-card",
    wrote: "message",
  })
  mod.session.observe({
    door: "prompt",
    kind: "append",
    message: { content: [{ text: "quoting row-card", type: "text" }], role: "user", type: "user" },
    origin: { kind: "plugin", name: "claude-envoy" },
    uuid: "row-pointer",
    wrote: "pointer",
  })
  // Another installed mod's append, which reaches this session under a `plugin` origin of its
  // own and carries no stamp, and a person's own prompt.
  mod.session.observe({
    door: "note",
    kind: "append",
    message: {
      content: [{ text: "another mod's context", type: "text" }],
      role: "user",
      type: "user",
    },
    origin: { kind: "plugin", name: "spike-mod2" },
    uuid: "row-elsewhere",
  })

  expect(mod.session.appendedRow("row-card")).toBe(true)
  expect(mod.session.appendedRow("row-elsewhere")).toBe(false)
  expect(mod.session.appendedRow("row-pointer")).toBe(false)
  expect(mod.session.runsUnreadPrompt("row-pointer")).toBe(true)
  expect(mod.session.runsUnreadPrompt("row-card")).toBe(false)
})

// A person's message takes the same one call. What differs is the answer: the row of an append
// is that Dispatch message's row, which the router records, while the row of a prompt is the
// prompt's own and the router recognises it by its text.
test("a person's message answers with the row an append wrote, and with none for a prompt", async () => {
  const stored = driven(() => ({ as: "row", uuid: "row-person" }))
  const delivered = { content: BLOCKS, messageId: MESSAGE, text: "ship it" }

  expect(await stored.session.deliver(delivered)).toBe("row-person")
  expect(stored.written[0]?.message).toEqual({
    kind: "person",
    messageId: MESSAGE,
    text: "ship it",
  })

  const prompted = driven(() => ({ as: "prompt", uuid: "prompt-row-1" }))
  expect(await prompted.session.deliver(delivered)).toBeUndefined()
})

// A prompt the session dropped is the person's to be told about: their own words were refused,
// and Dispatch is where they are waiting. A row it refused is this server's failure to write,
// and the frame is left for Envoy to send again.
test("a person's prompt the session dropped is reported, and a refused row throws", async () => {
  const dropped = driven(() => ({ as: "prompt", deny: "a UserPromptSubmit hook blocked it" }))
  const failures: unknown[] = []
  const delivered = { content: BLOCKS, messageId: MESSAGE, text: "ship it" }

  expect(
    await dropped.session.deliver({ ...delivered, failed: (error) => failures.push(error) }),
  ).toBeUndefined()
  await settled()
  expect(String(failures[0])).toContain("a UserPromptSubmit hook blocked it")

  const refused = driven(() => ({ as: "row", deny: "the conversation is compacting" }))
  await expect(
    refused.session.deliver({ ...delivered, failed: (error) => failures.push(error) }),
  ).rejects.toThrow("the conversation is compacting")
})
