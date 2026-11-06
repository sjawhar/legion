/**
 * What a card becomes in a Claude Code session (`ModSession.show`, INFRA-1026): an Envoy
 * delivery that is not the person's own message — an ask answer, a teammate's or an architect's
 * message, an issue event — takes the three states a person's message takes, all but `asUser`.
 *
 * A person's own message is driven through the router that accepts it
 * (`./user-turn-router.test.ts`); a card has no router, since the server writes it into the
 * session as it reads it (`writer` in `../src/envoy-channel-server`).
 */
import { expect, test } from "bun:test"
import type { ModCallArguments, ModCallName, ModCallResults, ModEvent } from "../hooks/mod-protocol"
import { cardPrompt, type ModSession, modSession, unreadRowPrompt } from "../src/mod-session"
import { settled, waitFor } from "./channel-session-harness"

/** A card as the server hands one over: the rendered delivery's text, and the blocks it is. */
const CARD = "Dispatch message from alice: the migration is approved."
const BLOCKS = [{ text: CARD, type: "text" }]
const MESSAGE = "33333333-3333-4333-8333-333333333333"

type Answer<N extends ModCallName> = (
  args: ModCallArguments[N],
) => ModCallResults[N] | Promise<ModCallResults[N]>

interface Driven {
  readonly session: ModSession
  madeOf<N extends ModCallName>(call: N): ModCallArguments[N][]
  /** Hands the session one event, as the live view does (`startAgentStream`). */
  observe(event: ModEvent): void
  /** Every call the session made, in the order it made them. */
  order(): ModCallName[]
}

/** A session on a mod that answers what the test says and remembers what it was asked. */
function driven(answers: { [N in ModCallName]?: Answer<N> } = {}): Driven {
  const calls: { call: ModCallName; args: unknown }[] = []
  const session = modSession({
    link: {
      connected: () => true,
      request<N extends ModCallName>(
        call: N,
        args: ModCallArguments[N],
      ): Promise<ModCallResults[N]> {
        calls.push({ call, args })
        const answer = answers[call]
        if (answer === undefined) return Promise.reject(new Error(`no answer for ${call}`))
        return Promise.resolve(answer(args))
      },
    },
    warn: () => undefined,
  })
  return {
    madeOf: <N extends ModCallName>(call: N): ModCallArguments[N][] =>
      calls.filter((made) => made.call === call).map((made) => made.args as ModCallArguments[N]),
    observe: (event) => session.observe(event),
    order: () => calls.map((made) => made.call),
    session,
  }
}

/** The snapshots and the end of a turn that read everything handed to it. */
function answers(mod: Driven, turnId: string, index: number): void {
  mod.observe({ kind: "step-request", turnId, index })
  mod.observe({ done: true, index, kind: "step", model: "claude-sonnet-4-5", parts: [], turnId })
  mod.observe({ kind: "turn-complete", stopReason: "answer", turnId })
}

// An append starts nothing: a row is read at the turn's next request, and an idle session makes
// none, so a card appended to one waited for the person to type (INFRA-1026 review). The
// approved decision, Oh My Pi's rule for the same card (`packages/pi-envoy/src/card-wake.ts`)
// and this package's own documentation all say the card starts the turn as well, and the row is
// stored before that prompt goes out: a submit is never awaited, so the append is the only part
// of this the delivery's frame can be answered on (the test below, and `hand` in
// `../src/mod-session`).
test("a card to an idle session is the prompt of a turn, and that turn answers it", async () => {
  const mod = driven({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => ({ uuid: "row-card" }),
  })
  await mod.session.show({ content: BLOCKS, text: CARD })

  // Stored first, started second.
  expect(mod.order()).toEqual(["session.append", "prompt.submit"])
  expect(mod.madeOf("session.append")).toEqual([{ message: { content: BLOCKS, type: "user" } }])
  const submits = mod.madeOf("prompt.submit")
  expect(submits).toHaveLength(1)
  // Without `asUser`: the person typed none of this, and a prompt claiming they did would put
  // words in their mouth.
  expect(submits[0]?.asUser).toBeUndefined()
  expect(submits[0]?.text).toBe(cardPrompt(CARD))
  expect(submits[0]?.text).toContain(CARD)

  // The turn that prompt started reads it and ends: nothing is pointed at it afterwards.
  mod.observe({ kind: "turn-start", turnId: "turn-1" })
  answers(mod, "turn-1", 0)
  await settled()
  expect(mod.madeOf("prompt.submit")).toHaveLength(1)
})

// What the Envoy frame's `done` rests on is `show` resolving, and a submit is never awaited: it
// resolves at the end of the turn it starts. Driven live (INFRA-1026, k7's second clause:
// Claude Code SIGSTOPped, an Envoy message sent to it from another session, then SIGKILL and
// `claude --resume <id>`), an idle branch that only submitted answered the frame although the
// prompt never entered the stopped process, and the resumed conversation held that message zero
// times where the branch that awaited the append held it once.
test("a card an idle session cannot take rejects, so its frame goes unanswered", async () => {
  const stopped = driven({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => Promise.reject(new Error("this session has no link to Claude Code")),
  })
  await expect(stopped.session.show({ content: BLOCKS, text: CARD })).rejects.toThrow(
    "this session has no link to Claude Code",
  )
  // Nothing was submitted either: the card is the session's to take or to refuse whole.
  expect(stopped.madeOf("prompt.submit")).toEqual([])

  // A session that answers the call and refuses the row is the same to the caller.
  const refusing = driven({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => ({ deny: "the conversation is compacting" }),
  })
  await expect(refusing.session.show({ content: BLOCKS, text: CARD })).rejects.toThrow(
    "the session refused the message: the conversation is compacting",
  )
  expect(refusing.madeOf("prompt.submit")).toEqual([])
})

test("the card's frame is answered once the row is stored, and not before", async () => {
  const stored = Promise.withResolvers<ModCallResults["session.append"]>()
  const mod = driven({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => stored.promise,
  })
  let answered = false
  const showing = mod.session.show({ content: BLOCKS, text: CARD }).then(() => {
    answered = true
  })

  await settled()
  expect(answered).toBe(false)
  // The prompt waits on the row too: the turn it starts is the one that reads that row.
  expect(mod.madeOf("prompt.submit")).toEqual([])

  stored.resolve({ uuid: "row-card" })
  await showing
  expect(answered).toBe(true)
  expect(mod.madeOf("prompt.submit")).toHaveLength(1)
})

test("a card during a turn is a row that turn reads, and no prompt of its own", async () => {
  const mod = driven({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => ({ uuid: "row-card" }),
  })
  mod.observe({ kind: "turn-start", turnId: "turn-1" })

  await mod.session.show({ content: BLOCKS, text: CARD })

  expect(mod.madeOf("session.append")).toEqual([{ message: { content: BLOCKS, type: "user" } }])
  expect(mod.madeOf("prompt.submit")).toEqual([])

  // A request went out after the row, which is the request that read it.
  answers(mod, "turn-1", 1)
  await settled()
  expect(mod.madeOf("prompt.submit")).toEqual([])
})

test("a card no request read is quoted by the prompt the turn's end submits", async () => {
  const mod = driven({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => ({ uuid: "row-card" }),
  })
  mod.observe({ kind: "turn-start", turnId: "turn-1" })
  mod.observe({ kind: "step-request", turnId: "turn-1", index: 0 })

  await mod.session.show({ content: BLOCKS, text: CARD })
  // No request went out after the row, so the model never read it.
  mod.observe({ kind: "turn-complete", stopReason: "answer", turnId: "turn-1" })
  await waitFor(async () => mod.madeOf("prompt.submit").length === 1, "the prompt at the card")

  const submits = mod.madeOf("prompt.submit")
  expect(submits).toHaveLength(1)
  expect(submits[0]?.asUser).toBeUndefined()
  expect(submits[0]?.text).toBe(unreadRowPrompt([{ kind: "card", text: CARD }]))
  expect(submits[0]?.text).toContain(CARD)
  // The prompt says who wrote it, which is nobody at this session.
  expect(submits[0]?.text).toContain("which nobody here typed")
})

// One submit per turn end, whatever went unread: a second would sit behind the first for a whole
// turn (`SUBMIT_TIMEOUT_MS` and the measurement above it, `../src/mod-session`).
test("a card and a person's message left unread by one turn share the one prompt", async () => {
  const rows: string[] = []
  const mod = driven({
    "prompt.submit": (args) => ({ text: args.text }),
    "session.append": () => {
      rows.push(`row-${rows.length}`)
      return { uuid: `row-${rows.length}` }
    },
  })
  mod.observe({ kind: "turn-start", turnId: "turn-1" })
  mod.observe({ kind: "step-request", turnId: "turn-1", index: 0 })

  await mod.session.show({ content: BLOCKS, text: CARD })
  await mod.session.deliver({
    content: [{ text: "ship it", type: "text" }],
    messageId: MESSAGE,
    text: "ship it",
  })
  mod.observe({ kind: "turn-complete", stopReason: "answer", turnId: "turn-1" })
  await waitFor(async () => mod.madeOf("prompt.submit").length === 1, "the prompt at both rows")

  const submits = mod.madeOf("prompt.submit")
  expect(submits).toHaveLength(1)
  expect(submits[0]?.text).toBe(
    unreadRowPrompt([
      { kind: "card", text: CARD },
      { kind: "person", messageId: MESSAGE, text: "ship it" },
    ]),
  )
  expect(submits[0]?.text).toContain(CARD)
  expect(submits[0]?.text).toContain("ship it")
  expect(submits[0]?.text).toContain(MESSAGE)
})
