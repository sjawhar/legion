// The rows Claude Code writes of its own bookkeeping, parsed (INFRA-1026).
//
// One command a person runs writes three rows by the `command` door - the caveat, the echo of
// what was typed, and the output - and two readers of this plugin parse them. The live view draws
// the echo row as the plain command line a person typed and drops the other two, which would
// otherwise show a reader raw tags and a `/context` table inside the agent's conversation
// (`./agent-stream-claude`). The session reads the output row to answer the person who sent the
// command, since most commands resolve `$.command.run` with no output in the result at all
// (`./mod-session`, `runCommand`).
//
// A picture a person sent writes a row of the same kind: Claude Code's note of where it put the
// bytes (`isPictureAttachmentRow`), which the live view drops as it drops the command's
// bookkeeping.
//
// A line a person types while a turn is running arrives wrapped in framing of Claude Code's own
// (`claudeMidTurnMessage`), and the live view draws the person's words out of it and none of the
// wrapper.
import type { ModContentBlock } from "../hooks/mod-protocol"

/**
 * The tags Claude Code wraps its own bookkeeping of a slash command in.
 *
 * Measured on 2.1.310: a command a person runs writes three rows by the `command` door - the
 * caveat row, which says the command's output went to the person rather than to the model and
 * carries `isMeta: true`; the echo row, which holds what was typed; and the output row. The
 * output row's `message.type` is `user` in some runs and `system` (`name: "local_command"`) in
 * others, so the tag its text starts with is the only safe discriminator
 * (`/legion/scratch/tmp/infra-1026-spike/config/projects/-legion-scratch-tmp-infra-1026-proof-work/22a2c318-8433-4659-9568-a5ce1a873926.jsonl:42-44`,
 * this plugin's own mod running `/usage`).
 */
const COMMAND_BOOKKEEPING_TAGS = [
  "<local-command-caveat>",
  "<local-command-stdout>",
  "<local-command-stderr>",
] as const

/** The two tags an output row wraps what the command printed in, in the order a row carrying
 *  both would be read: what went to the terminal's stdout, else what went to its stderr. */
const COMMAND_OUTPUT_TAGS = ["local-command-stdout", "local-command-stderr"] as const

/** The text a row's content holds, blocks flattened: a tool result is shown as text alone. A
 *  block with no text of its own is named rather than dropped, as the publisher names one. */
export function claudeRowText(value: unknown): string {
  if (typeof value === "string") return value
  if (!Array.isArray(value)) return ""
  const pieces: string[] = []
  for (const item of value as readonly unknown[]) {
    if (typeof item === "string") {
      pieces.push(item)
      continue
    }
    if (typeof item !== "object" || item === null) continue
    const block = item as Record<string, unknown>
    const text = block["text"]
    const type = block["type"]
    if (typeof text === "string") pieces.push(text)
    else if (typeof type === "string") pieces.push(`[${type}]`)
  }
  return pieces.join("\n")
}

/** The value one of a row's tags holds, trimmed: the recorded row indents every tag after the
 *  first, so the whitespace around a value is the row's layout and not part of it. */
function taggedValue(text: string, tag: string): string | undefined {
  const open = `<${tag}>`
  const from = text.indexOf(open)
  if (from === -1) return undefined
  const to = text.indexOf(`</${tag}>`, from + open.length)
  if (to === -1) return undefined
  return text.slice(from + open.length, to).trim()
}

/**
 * The plain command line a row of the `command` door echoes, or `undefined` where the row is not
 * the echo of a command a person ran.
 *
 * Measured on 2.1.310: the echo row holds one text block naming `<command-name>/usage</command-
 * name>`, `<command-message>usage</command-message>` and `<command-args></command-args>`,
 * newline-separated, each tag on its own line (same transcript as
 * `COMMAND_BOOKKEEPING_TAGS`). The command line is the name, and the arguments after a space
 * where there are any - `/clear`, `/effort low` - which is what the person typed and all of the
 * command the live view shows.
 */
export function claudeEchoedCommandLine(content: readonly ModContentBlock[]): string | undefined {
  const text = claudeRowText(content)
  const name = taggedValue(text, "command-name")
  if (name === undefined || name === "") return undefined
  const args = taggedValue(text, "command-args") ?? ""
  return args === "" ? name : `${name} ${args}`
}

/**
 * What the output row of a slash command printed, or `undefined` where the row is not an output
 * row. A row that is one and holds nothing answers `""`: the command printed nothing, which is
 * itself the answer the person who sent it gets.
 *
 * Measured on 2.1.310: the row holds one text block wrapped in
 * `<local-command-stdout>…</local-command-stdout>` (`<local-command-stderr>` where the command
 * wrote to stderr), and most commands put their whole output there and nowhere else -
 * `/effort low` resolves `$.command.run` with `{}` and writes
 * `<local-command-stdout>Set effort level to low …</local-command-stdout>` 158 ms later
 * (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q2-310.jsonl:98-102`).
 */
export function claudeCommandOutput(content: readonly ModContentBlock[]): string | undefined {
  const text = claudeRowText(content)
  for (const tag of COMMAND_OUTPUT_TAGS) {
    const value = taggedValue(text, tag)
    if (value !== undefined) return value
  }
  return undefined
}

/** Whether a row of the `command` door is Claude Code's bookkeeping of the command rather than
 *  anything a person wrote: the caveat it records before the command runs, and the command's own
 *  output, which published shows the person raw tags and a `/context` table in the agent's
 *  conversation (INFRA-1026). */
export function isCommandBookkeeping(content: readonly ModContentBlock[]): boolean {
  const text = claudeRowText(content)
  return COMMAND_BOOKKEEPING_TAGS.some((tag) => text.startsWith(tag))
}

/**
 * The row Claude Code writes of where it put a picture's bytes: one text block holding
 * `[Image: source: <absolute path>]` and nothing else.
 *
 * Measured on 2.1.310: a person's message carrying a picture reaches the session as its text and
 * an image block, and Claude Code writes the bytes under the session's own images directory and
 * records a companion row for them - `type: "user"`, `isMeta: true`, `turnCompanion: true`, its
 * one text block
 * `[Image: source: /legion/scratch/tmp/i3-k5-pane/tmp/claude-1000/-legion-scratch-tmp-i3-k5-pane-cwd/f376a3a9-6e75-461b-806b-22dea4e83ddf/images/1.png]`
 * (`/legion/scratch/tmp/i3-k5-pane/config/projects/-legion-scratch-tmp-i3-k5-pane-cwd/f376a3a9-6e75-461b-806b-22dea4e83ddf.jsonl:123`,
 * a Legion pane answering a picture sent from the Agents page). The path is Claude Code saying
 * where the bytes went, not a message anybody wrote, so the row is no part of the conversation.
 */
const PICTURE_ATTACHMENT_ROW = /^\[Image: source: \/[^\]]+\]$/

/** Whether a row is that note of Claude Code's rather than anything a person wrote
 *  (`PICTURE_ATTACHMENT_ROW`), so the live view drops it as it drops a command's bookkeeping:
 *  drawn, it is a second user row for the one message the person sent (INFRA-1026). */
export function isPictureAttachmentRow(content: readonly ModContentBlock[]): boolean {
  return PICTURE_ATTACHMENT_ROW.test(claudeRowText(content).trim())
}

/** The tags and the two fixed sentences the wrapper around a mid-turn message is made of
 *  (`claudeMidTurnMessage`): the opening tag, the announcement, the person's words, Claude
 *  Code's note to the model, the closing tag. */
const MID_TURN_OPEN = "<system-reminder>"
const MID_TURN_CLOSE = "</system-reminder>"
const MID_TURN_ANNOUNCEMENT = "The user sent a new message while you were working:"
const MID_TURN_NOTE = "This is how Claude Code surfaces messages the user sends mid-turn"

/**
 * The words a person typed while a turn was running, read out of the wrapper Claude Code hands
 * them in, or `undefined` where the text is not that wrapper.
 *
 * Measured on 2.1.310: a line typed at the terminal mid-turn reaches the mod as one row by the
 * `delivery` door (`origin.kind: "composer"`, `message.type: "attachment"`,
 * `name: "queued_command"`, `/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:120`) whose
 * single text block is
 *
 *     <system-reminder>
 *     The user sent a new message while you were working:
 *     TYPED-AT-TERMINAL-h1: say the word CEDAR when you can.
 *
 *     This is how Claude Code surfaces messages the user sends mid-turn - within the running
 *     turn, often alongside the next tool result, rather than as a separate conversation turn.
 *     Address the message above as you continue this turn.
 *     </system-reminder>
 *
 * (`/legion/scratch/tmp/t-term-h1/config/projects/-legion-scratch-tmp-t-term-h1-cwd/b6411755-7568-4608-93a5-1db3c839e038.jsonl:70`).
 * Drawn whole, that is tags and a note written to the model where the person wrote one sentence,
 * which is the plumbing the view already drops for a command and for a picture's bytes.
 *
 * The words are found by the wrapper's shape and never by line number, since a person's message
 * runs to as many lines, and as many blank ones, as they typed: everything between the
 * announcement and the note is theirs. The note is looked for from the end, so a person who
 * quotes its first words keeps their own. A wrapper holding no words answers `""`, which shows
 * the reader nothing - right, because none of the framing is anybody's words.
 *
 * A `<system-reminder>` of any other kind answers `undefined` and the view draws it as it draws
 * any other text row today, which this reader leaves alone: measured in the same spike, a peer
 * session's message (`logs/mod3-q1q3q5.jsonl:223`) and the
 * `[SYSTEM NOTIFICATION - NOT USER INPUT]` rows (`logs/mod3-q7-int.jsonl:13`) come by this same
 * `delivery` door, and `<total_tokens>` by the `attachment` door the mod does not watch
 * (`logs/mod2.jsonl:121`).
 */
export function claudeMidTurnMessage(text: string): string | undefined {
  const body = text.trim()
  if (!body.startsWith(MID_TURN_OPEN) || !body.endsWith(MID_TURN_CLOSE)) return undefined
  const inner = body.slice(MID_TURN_OPEN.length, body.length - MID_TURN_CLOSE.length).trimStart()
  if (!inner.startsWith(MID_TURN_ANNOUNCEMENT)) return undefined
  const note = inner.lastIndexOf(MID_TURN_NOTE)
  if (note < MID_TURN_ANNOUNCEMENT.length) return undefined
  return inner.slice(MID_TURN_ANNOUNCEMENT.length, note).trim()
}
