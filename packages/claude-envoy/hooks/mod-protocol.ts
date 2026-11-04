/**
 * The wire between the Claude Code mod (`./mod.ts`, loaded from source by the `modules` key of
 * `./hooks.json`) and the socket server inside this plugin's MCP server (`../src/mod-link.ts`):
 * where the socket lives, what crosses it, and nothing else.
 *
 * This module must stay import-free. A mod runs in an environment of its own: it cannot import a
 * `node:` builtin or an npm package, and anything `./mod.ts` imports must be another file under
 * `hooks/` that itself imports nothing at run time. The MCP server imports the same file
 * (`../hooks/mod-protocol`) and bun inlines it into the bundle, so the two sides can never
 * disagree about a route, a shape or the socket's path. Keep every value here a plain constant or
 * a pure function over its arguments; read no global, not even `process` or `TextEncoder`.
 */

/** The body `GET /healthz` answers: `{ "link": MOD_HEALTH_MARKER }`. A socket that answers this
 *  marker has a live server behind it, which is how the server tells a stale socket from one
 *  another server owns, and how the mod knows it reached its own server and not something else. */
export const MOD_HEALTH_MARKER = "claude-envoy-mod"

/** How long the server holds a `POST /requests` open before answering an empty list. Short on
 *  purpose: `$.http.fetch` has no timeout field and the host's own timeout is unknown, so a hold
 *  no host could cut short is worth more than a long poll's fewer round trips. */
export const MOD_POLL_HOLD_MS = 5_000

/** The Claude Code release the mod needs: below it the mod never connects, and the session runs as
 *  if the plugin shipped no mod at all. 2.1.310 is the release the link was measured on; an older
 *  Claude Code either does not know the `modules` key (it ignores it silently) or does not serve
 *  the `$` calls and hooks `./mod.ts` is written against. */
export const MOD_MIN_CLAUDE_VERSION = "2.1.310"

/** The URL every request of the mod's goes to. Over a Unix socket the host part is nothing but the
 *  `Host` header, and `$.http.fetch` still needs a whole URL, so the link names itself there. */
export const MOD_URL_BASE = "http://claude-envoy"

/** The liveness probe, `GET`. Answers 200 `{ link: MOD_HEALTH_MARKER }`. */
export const MOD_ROUTE_HEALTH = "/healthz"

/** The mod's events, `POST` `{ events }`; answers 200 `{ ok: true }`. Fire and forget. */
export const MOD_ROUTE_EVENTS = "/events"

/** The mod's answers to the server's requests, `POST` `{ results }`; answers 200 `{ ok: true }`. */
export const MOD_ROUTE_RESULTS = "/results"

/** The mod's poll for work, `POST`; answers 200 `{ requests }`, held up to `MOD_POLL_HOLD_MS`. */
export const MOD_ROUTE_REQUESTS = "/requests"

/** The longest socket path either side will use, in bytes. Claude Code's `HttpInit.socketPath`
 *  says "near 100 B at most"; Linux's own `sun_path` holds 108. The smaller of the two bounds is
 *  the one that has to hold, since a path the host refuses is no use however well it binds. */
export const MOD_SOCKET_PATH_MAX_BYTES = 100

/** The variables that may name the base the socket sits in, in the order they are tried. The mod
 *  spells each one as a literal of its own, since `$.env.get` takes no computed name. */
const MOD_SOCKET_BASE_VARIABLES = ["XDG_RUNTIME_DIR", "TMPDIR"] as const

/** The base used when no variable names one that fits. */
const MOD_SOCKET_FALLBACK_BASE = "/tmp"

/** Where the socket for `sessionId` is, or why there is none. One of the two, never both. */
export type ModSocketResolution =
  | { readonly path: string; readonly refusal?: undefined }
  | { readonly path?: undefined; readonly refusal: string }

/** `value`'s length in bytes once encoded as UTF-8, counted here rather than through a
 *  `TextEncoder` so this module keeps to plain arithmetic over its argument. Iterating a string
 *  walks it by code point, so a surrogate pair is counted once, as four bytes. */
function utf8Length(value: string): number {
  let bytes = 0
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0
    bytes += code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4
  }
  return bytes
}

/** Whether `value` is one plain path segment: something that names a file inside a directory and
 *  cannot leave it. Claude Code's session ids are UUIDs, so anything else is a mistake worth
 *  refusing rather than encoding: an encoded id would still have to round-trip through the mod. */
function isPathSegment(value: string): boolean {
  return (
    value.length > 0 &&
    value !== "." &&
    value !== ".." &&
    !value.includes("/") &&
    !value.includes("\\") &&
    !value.includes("\0")
  )
}

/** `base` with its trailing slashes dropped, so joining never doubles one; the root stays "". */
function withoutTrailingSlash(base: string): string {
  let end = base.length
  while (end > 1 && base[end - 1] === "/") end -= 1
  return end === 1 ? "" : base.slice(0, end)
}

/**
 * Where the mod and the server meet for `sessionId`, or why they cannot.
 *
 * The path is `<base>/claude-envoy-<sessionId>.sock`. `<base>` is the first of `XDG_RUNTIME_DIR`,
 * `TMPDIR` and `/tmp` that is set to a non-empty absolute path whose whole resulting path fits in
 * `MOD_SOCKET_PATH_MAX_BYTES`: a run directory that is too deep is passed over rather than
 * truncated, since a truncated path is another session's socket.
 */
export function resolveModSocketPath(
  sessionId: string,
  env: Readonly<Record<string, string | undefined>>,
): ModSocketResolution {
  if (!isPathSegment(sessionId)) {
    return {
      refusal: `${JSON.stringify(sessionId)} is not a plain path segment, so it names no socket of its own`,
    }
  }
  const file = `/claude-envoy-${sessionId}.sock`
  const bases = [...MOD_SOCKET_BASE_VARIABLES.map((name) => env[name]), MOD_SOCKET_FALLBACK_BASE]
  const tried: string[] = []
  for (const base of bases) {
    if (base === undefined || base.length === 0 || !base.startsWith("/")) continue
    const path = `${withoutTrailingSlash(base)}${file}`
    if (utf8Length(path) <= MOD_SOCKET_PATH_MAX_BYTES) return { path }
    tried.push(base)
  }
  // `/tmp` is absolute, so at least it was tried: there is no case where nothing was.
  return {
    refusal: `every base (${tried.join(", ")}) makes a socket path longer than ${MOD_SOCKET_PATH_MAX_BYTES} bytes`,
  }
}

/** The socket path for `sessionId`, or undefined when there is none: the short view of
 *  `resolveModSocketPath`, whose `refusal` says why a caller has nothing to bind or connect to. */
export function modSocketPath(
  sessionId: string,
  env: Readonly<Record<string, string | undefined>>,
): string | undefined {
  return resolveModSocketPath(sessionId, env).path
}

/** The numeric core of a Claude Code version (`2.1.310`, `2.1.310-dev.20260920.t101500.sha1a2b3`):
 *  the dotted numbers it opens with, stopping at the first part that is not one, so a release and
 *  a development build of it have the same core. */
function versionParts(version: string): readonly number[] {
  const parts: number[] = []
  for (const part of version.split(".")) {
    const digits = /^\d+/.exec(part)
    if (digits === null) break
    const [number] = digits
    parts.push(Number(number))
    // `310-dev` is the core's last part: what follows the digits is the channel, not a number.
    if (number.length !== part.length) break
  }
  return parts
}

/**
 * Whether `version` is `minimum` or newer, comparing the numeric cores alone.
 *
 * A development build of the minimum (`2.1.310-dev.20260920...`) counts as the minimum: its core
 * is the release it is built from, and refusing it would refuse the build the link is developed
 * on. A version with no numeric core at all is refused, since nothing can be said about it.
 */
export function modVersionAtLeast(version: string, minimum: string): boolean {
  const left = versionParts(version)
  const right = versionParts(minimum)
  if (left.length === 0) return false
  for (let index = 0; index < Math.max(left.length, right.length); index += 1) {
    const mine = left[index] ?? 0
    const theirs = right[index] ?? 0
    if (mine !== theirs) return mine > theirs
  }
  return true
}

/** How hard a request asks the model to think, as `turn.step` carries it (`TurnStepInput.effort`
 *  in Claude Code's declarations). */
export type ModEffort = "low" | "medium" | "high" | "xhigh" | "max" | number

/** Why the model stopped, as a `turn.step` result carries it (`TurnStopReason`); null when no
 *  response arrived. */
export type ModStopReason =
  | "end_turn"
  | "max_tokens"
  | "stop_sequence"
  | "tool_use"
  | "pause_turn"
  | "compaction"
  | "refusal"
  | "model_context_window_exceeded"
  | null

/** Why a turn ended, as `turn.complete` carries it (`TurnCompleteReason`). */
export type ModTurnReason = "answer" | "aborted" | "refusal" | "error"

/** Why the session ended, as `session.end` carries it (`SessionEndReason`). */
export type ModSessionEndReason = "clear" | "resume" | "logout" | "prompt_input_exit" | "other"

/** Which door a row of the conversation came in by (`SessionAppendDoor`), of the ones the mod
 *  watches. The doors it leaves alone are `attachment`, `hook-context` and `compaction`, which
 *  Claude Code's built-in guard hides from a mod a person installs, and `queue`, which is no row
 *  of the conversation: matching any of them would have the guard write a skipped-hook notice
 *  into what the model reads. */
export type ModAppendDoor =
  | "prompt"
  | "command"
  | "response"
  | "tool-result"
  | "tool-message"
  | "delivery"
  | "note"
  | "notice"

/** Every door the mod watches, which is every door a row of the conversation comes in by. The
 *  mod spells each one again at its own `on("session.append", { door }, ...)`, since the host
 *  reads those matchers off the source; this list is what a reader of a row checks against. */
export const MOD_APPEND_DOORS: readonly ModAppendDoor[] = [
  "prompt",
  "command",
  "response",
  "tool-result",
  "tool-message",
  "delivery",
  "note",
  "notice",
]

/** One block of a row, as the Messages API shapes it (`ApiContentBlock`): its `type` and whatever
 *  that type carries. The link relays a block whole and reads none of it. */
export interface ModContentBlock {
  readonly type: string
  readonly [field: string]: unknown
}

/** One row of the conversation as `session.append` hands it (`SessionAppendMessage`). */
export interface ModAppendMessage {
  readonly type: "user" | "assistant" | "attachment" | "system"
  /** An attachment's type or a notice's subtype; absent on user and assistant rows. */
  readonly name?: string
  /** Under which role a request carries the row; absent when none does. */
  readonly role?: "user" | "assistant"
  /** True on a user-side row the person does not see as typed. */
  readonly isMeta?: true
  readonly content: readonly ModContentBlock[]
}

/** Who caused a row (`SessionAppendOrigin`): its `kind`, and the one field that kind carries.
 *  The kinds are a long open-ended list in Claude Code's declarations and a new build adds to it,
 *  so the kind crosses as the string it is. */
export interface ModAppendOrigin {
  readonly kind: string
  /** `plugin`: the submitting plugin's name. */
  readonly name?: string
  /** `model`: whose response it is. */
  readonly model?: string
  /** `tool`: which tool was called. */
  readonly tool?: string
  /** `hook`: the settings hook event that produced it. */
  readonly event?: string
  /** `channel`: the channel server's name. */
  readonly server?: string
}

/** One part of a `turn.step` snapshot: the text the model wrote, the thinking it showed, or a tool
 *  call it began, in the order their blocks opened. */
export type ModStepPart =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "reasoning"; readonly text: string }
  | {
      readonly type: "tool-call"
      /** The call's `tool_use_id`. */
      readonly id: string
      /** The tool's name (`Read`, `Bash`, `mcp__server__tool`). */
      readonly name: string
      /** The arguments as JSON text, the pieces that have arrived concatenated: partial while the
       *  block streams, whole once it closes. Text rather than a parsed object because a half
       *  arrived argument list does not parse, and the live view shows it as it arrives. */
      readonly args: string
    }

/** Something that happened in the session, sent by the mod. A later task turns these into the
 *  Agents page's live view; nothing here knows about that. */
export type ModEvent =
  /** The mod reached the server: sent once per connect. */
  | {
      readonly kind: "hello"
      /** `$.session.id()` as the mod's first `session.start` read it: the id the socket is named
       *  for, which stays this process's whatever a later `/clear` does. */
      readonly sessionId: string
      /** `$.session.version()`'s `version`. */
      readonly claudeVersion: string
      /** Whether a person is at the prompt (`SessionStartInput.isInteractive`). */
      readonly interactive: boolean
    }
  /** The session's id changed: a `/clear` ended the conversation and the process went on under a
   *  new one. The socket does not move with it. */
  | { readonly kind: "session-id"; readonly sessionId: string }
  | { readonly kind: "session-end"; readonly reason: ModSessionEndReason }
  | { readonly kind: "turn-start"; readonly turnId: string }
  /** One snapshot of a `turn.step`, at most ten a second while it streams and once more when it
   *  ends. `parts` is the whole step so far, not a delta. */
  | {
      readonly kind: "step"
      readonly turnId: string
      /** The step's position in the turn, from 0. */
      readonly index: number
      readonly model: string
      /** Absent for a model without effort, as `TurnStepInput.effort` is. */
      readonly effort?: ModEffort
      readonly parts: readonly ModStepPart[]
      /** True on the one snapshot sent after the response is whole. */
      readonly done: boolean
      /** Why the model stopped; only on the snapshot with `done`. */
      readonly stopReason?: ModStopReason
    }
  /** The turn ended. `stopReason` is `turn.complete`'s own `reason`. */
  | { readonly kind: "turn-complete"; readonly turnId: string; readonly stopReason: ModTurnReason }
  /** One row of the conversation, as `session.append` handed it. */
  | {
      readonly kind: "append"
      readonly uuid: string
      readonly door: ModAppendDoor
      readonly origin: ModAppendOrigin
      readonly message: ModAppendMessage
    }
  /** The session's model, as the mod last read it (`$.session.model()`). */
  | { readonly kind: "model"; readonly model: string }

/** A `/config` row's value (`ConfigValue`). */
export type ModConfigValue = boolean | string | number | readonly string[]

/** One `/config` row as `$.config.list()` answers it (`ConfigRow`). `provider.tier` crosses as a
 *  string: Claude Code's tiers are its own list and the link reads none of them. */
export interface ModConfigRow {
  readonly key: string
  readonly label: string
  readonly description?: string
  /** How the row takes its value (`ConfigKind`). */
  readonly kind: "boolean" | "choice" | "text" | "number"
  readonly value: ModConfigValue
  readonly options?: readonly string[]
  readonly provider: { readonly plugin: string; readonly tier: string }
  readonly isLocked: boolean
}

/** One slash command as `$.command.list()` answers it (`CommandInfo`). */
export interface ModCommandInfo {
  readonly name: string
  readonly description: string
  /** Where it comes from (`CommandSource`). */
  readonly source: "builtin" | "plugin" | "user" | "mcp"
  readonly plugin?: string
}

/** The argument of each call the server may ask the mod to make. `undefined` for a call that takes
 *  none, so every request carries the key and no arm is special. */
export interface ModCallArguments {
  /** `$.session.append(args)` (`SessionAppendArgs`): a user row the model reads, or a system
   *  notice it never does. */
  "session.append": {
    readonly message: {
      readonly type: "user" | "system"
      readonly content: readonly ModContentBlock[]
    }
    /** The running subagent whose conversation the row joins; absent for main. */
    readonly agentId?: string
  }
  /** `$.prompt.submit(args)` (`PromptSubmitArgs`). Attachments are left out: a message from
   *  Dispatch is text, and the engine's media handles never leave the session. */
  "prompt.submit": {
    readonly text: string
    /** Submit the text as the person's own words, read without the plugin's frame. */
    readonly asUser?: true
  }
  /** `$.command.run(args)` (`CommandRunArgs`). */
  "command.run": { readonly command: string; readonly args?: string }
  /** `$.model.fork(request)` (`ModelForkRequest`). */
  "model.fork": { readonly prompt: string }
  /** `$.config.set(args)` (`ConfigSetArgs`). */
  "config.set": { readonly key: string; readonly value: ModConfigValue }
  /** `$.session.model()`. */
  "session.model": undefined
  /** `$.config.list()`. */
  "config.list": undefined
  /** `$.command.list()`. */
  "command.list": undefined
  /** Not a `$` call: the effort the mod puts on every later `turn.step` request, or null to stop
   *  putting one on and leave the session's own. */
  "set-effort": { readonly effort: ModEffort | null }
}

/** What each call answers with. */
export interface ModCallResults {
  /** The stored row's id. The row itself reaches the owner as an `append` event, so it is not
   *  sent twice. */
  "session.append": { readonly uuid: string; readonly deny?: undefined } | { readonly deny: string }
  /** The prompt that entered, or why it did not (`PromptSubmitResult`). */
  "prompt.submit": { readonly text: string; readonly drop?: undefined } | { readonly drop: string }
  /** The command's output and the notes it left the model (`CommandRunResult`). */
  "command.run": { readonly text?: string; readonly context?: readonly string[] }
  /** The fork's reply, or why there was none (`ModelForkResult`); its usage is not relayed. */
  "model.fork":
    | { readonly isAnswered: true; readonly text: string }
    | { readonly isAnswered: false; readonly reason: string }
  /** The value once written, or why not (`ConfigSetResult`). */
  "config.set":
    | { readonly value: ModConfigValue; readonly deny?: undefined }
    | { readonly deny: string }
  /** The session's model id. */
  "session.model": string
  "config.list": readonly ModConfigRow[]
  "command.list": readonly ModCommandInfo[]
  /** The effort now in force, as the mod holds it. */
  "set-effort": { readonly effort: ModEffort | null }
}

/** The name of a call the server may ask the mod to make. */
export type ModCallName = keyof ModCallArguments

/** One piece of work for the mod: its id, which call, and that call's argument. */
export type ModRequest = {
  readonly [N in ModCallName]: {
    readonly id: string
    readonly call: N
    readonly args: ModCallArguments[N]
  }
}[ModCallName]

/** What any call may answer with, for a reader that does not know which call it was. */
export type ModResultValue = ModCallResults[ModCallName]

/** The mod's answer to one request: the call's own value, or why it has none. */
export type ModResult =
  | { readonly id: string; readonly ok: true; readonly value: ModResultValue }
  | { readonly id: string; readonly ok: false; readonly error: string }

/** The body of `GET /healthz`. */
export interface ModHealthBody {
  readonly link: string
}

/** The body of `POST /events`. */
export interface ModEventsBody {
  readonly events: readonly ModEvent[]
}

/** The body of `POST /results`. */
export interface ModResultsBody {
  readonly results: readonly ModResult[]
}

/** What `POST /requests` answers: the work waiting, possibly none. */
export interface ModRequestsBody {
  readonly requests: readonly ModRequest[]
}

/** What `POST /events` and `POST /results` answer. */
export interface ModAckBody {
  readonly ok: true
}
