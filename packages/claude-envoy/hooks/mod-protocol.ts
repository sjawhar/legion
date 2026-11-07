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

/** The status a post gets when another mod instance already holds this link: one server serves
 *  one mod. Two Claude Code processes can boot on one session id (`claude -c`, and
 *  `claude --resume <id>` in a second terminal), and sharing one server would put both
 *  conversations in one live view and hand a person's Send to whichever mod polled first
 *  (INFRA-1026 review). The refused mod stops rather than retrying. */
export const MOD_STATUS_TAKEN = 409

/** The header every post of the mod's carries: the mod instance that sent it, minted once per
 *  loaded module. The first instance to name itself holds the link and every other is refused
 *  with `MOD_STATUS_TAKEN`. A post naming no instance claims nothing and is served, since the
 *  socket's directory is the user's own and this header decides between two mods, not between
 *  a mod and a stranger. */
export const MOD_INSTANCE_HEADER = "x-claude-envoy-mod"

/** The variables the socket's base is taken from, each spelled as a literal of its own since the
 *  mod reads them with `$.env.get`, which takes no computed name. The base is the user's run
 *  directory, and failing that Claude Code's own configuration directory: `CLAUDE_CONFIG_DIR`,
 *  else `<HOME>/.claude`. Nothing else is a base, `TMPDIR` and `/tmp` included: in a directory
 *  other local users can write, another of them binds the path first - a session id is not
 *  secret - and then reads the conversation and submits prompts as the person, or parks a plain
 *  file there and silences the session for good (INFRA-1026 review). A session none of the three
 *  variables names a base for gets no mod link at all. */
const MOD_SOCKET_RUNTIME_VARIABLE = "XDG_RUNTIME_DIR"
const MOD_SOCKET_CONFIG_VARIABLE = "CLAUDE_CONFIG_DIR"
const MOD_SOCKET_HOME_VARIABLE = "HOME"

/** Claude Code's configuration directory inside a home, which is where it keeps its own state
 *  when `CLAUDE_CONFIG_DIR` names no other (`../src/envoy-channel-server.ts` reads the same
 *  pair for this plugin's envoy.json). */
const MOD_SOCKET_HOME_CONFIG = "/.claude"

/** This plugin's own name, as its manifest spells it (`.claude-plugin/plugin.json`) and as
 *  Claude Code stamps it on a row of this plugin's: a prompt row of this plugin arrived live on
 *  2.1.310 under `{"kind": "plugin", "name": "claude-envoy"}`, and with `"asUser": true` where
 *  the prompt carried a person's own words (`/legion/scratch/tmp/v-run.log:218,229`). The mod
 *  names itself the same way on the rows it reports itself, which is what tells its own
 *  appended row from another installed mod's (`./mod.ts`, `../src/mod-session.ts`). */
export const MOD_PLUGIN_NAME = "claude-envoy"

/** The directory under the base that holds this user's sockets, named for the plugin. No other
 *  local user can create anything inside a home or a run directory, which is where ssh's
 *  `ControlPath` and gpg-agent put theirs and is why no co-user can plant this directory and
 *  get the socket; the server checks the directory it finds is its own and `0700` all the same
 *  before it binds (`../src/mod-link.ts`). The mod makes no such check, and could not:
 *  `$.fs.stat` answers `{kind, size, mtimeMs, isLink, realPath?}` (`FsStat` in Claude Code
 *  2.1.310's declarations) and carries neither owner nor mode. */
const MOD_SOCKET_DIRECTORY = MOD_PLUGIN_NAME

/** FNV-1a's prime, 16777619, in the two halves `fnv1a32` multiplies by. */
const MOD_HASH_PRIME_HIGH = 0x0100
const MOD_HASH_PRIME_LOW = 0x0193

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

/** FNV-1a over `value`'s UTF-16 code units, from `basis`, as 32 bits. The two sides name the
 *  socket with this one function, so the units it walks need not be the UTF-8 bytes the
 *  original is defined over. */
function fnv1a32(value: string, basis: number): number {
  let hash = basis
  for (let index = 0; index < value.length; index += 1) {
    hash = (hash ^ value.charCodeAt(index)) >>> 0
    // FNV's multiply, kept exact: a 32-bit product passes the 53 bits a double holds and loses
    // the low bits the hash is made of, so it is taken in halves whose own products do not, the
    // way `Math.imul` is written for an engine without it (this module reads no global, `Math`
    // included).
    const low = hash & 0xffff
    const high = hash >>> 16
    const carried = (high * MOD_HASH_PRIME_LOW + low * MOD_HASH_PRIME_HIGH) & 0xffff
    hash = (low * MOD_HASH_PRIME_LOW + (carried << 16)) >>> 0
  }
  return hash
}

/** The socket's name for `sessionId`: sixteen hex of two FNV-1a passes from different offsets,
 *  which is two 32-bit hashes rather than one repeated. A collision would hand two of one
 *  user's live sessions a single socket; at 64 bits over the sockets a machine holds at once,
 *  that is not a case worth designing for. */
function hashedSessionId(sessionId: string): string {
  const first = fnv1a32(sessionId, 0x811c9dc5)
  const second = fnv1a32(sessionId, 0xcbf29ce4)
  return `${first.toString(16).padStart(8, "0")}${second.toString(16).padStart(8, "0")}`
}

/** Whether `value` is one plain path segment. Claude Code's session ids are UUIDs, so anything
 *  else is a bug upstream of here: the hash below would make a usable name of it all the same,
 *  which is why it is refused rather than quietly named. */
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

/** `value` as a base to join onto, or undefined when it names none: a variable that is unset,
 *  empty or relative names no directory this resolution can be sure of. */
function absoluteBase(value: string | undefined): string | undefined {
  if (value === undefined || value.length === 0 || !value.startsWith("/")) return undefined
  return withoutTrailingSlash(value)
}

/** The bases this session's socket may sit in, in the order they are tried: the run directory,
 *  then Claude Code's configuration directory, which is `CLAUDE_CONFIG_DIR` when that names one
 *  and `<HOME>/.claude` otherwise. A home is read only for the configuration directory inside
 *  it, never as a base of its own. */
function socketBases(env: Readonly<Record<string, string | undefined>>): readonly string[] {
  const bases: string[] = []
  const runtime = absoluteBase(env[MOD_SOCKET_RUNTIME_VARIABLE])
  if (runtime !== undefined) bases.push(runtime)
  const configured = absoluteBase(env[MOD_SOCKET_CONFIG_VARIABLE])
  const home = absoluteBase(env[MOD_SOCKET_HOME_VARIABLE])
  if (configured !== undefined) bases.push(configured)
  else if (home !== undefined) bases.push(`${home}${MOD_SOCKET_HOME_CONFIG}`)
  return bases
}

/**
 * Where the mod and the server meet for `sessionId`, or why they cannot.
 *
 * The path is `<base>/claude-envoy/ce-<hash of sessionId>.sock`, and `<base>` is
 * `XDG_RUNTIME_DIR`, else Claude Code's configuration directory (`CLAUDE_CONFIG_DIR`, else
 * `<HOME>/.claude`). Both are directories only their own user can create anything in, the way
 * ssh's `ControlPath` and gpg-agent place theirs, so no co-user can get to the path first.
 *
 * A base whose whole path would pass `MOD_SOCKET_PATH_MAX_BYTES` is passed over rather than
 * truncated, since a truncated path is another session's socket; a session left with no base is
 * refused with that reason, never served out of a shared one.
 *
 * The name is the id's hash rather than the id itself because a uuid spelled out costs 36 bytes
 * more and passed the bound on a macOS temp directory (INFRA-1026 review). With the 38 bytes the
 * hashed name costs (`/claude-envoy/ce-` + 16 hex + `.sock`), the two real bases measured for
 * this rule are far inside the 100: a macOS `/Users/alexandra/.claude` (24) makes 62, and a
 * Legion pane's `CLAUDE_CONFIG_DIR` of `/legion/scratch/state/implementer/claude` (40) makes 78.
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
  const file = `/${MOD_SOCKET_DIRECTORY}/ce-${hashedSessionId(sessionId)}.sock`
  const tried: string[] = []
  for (const base of socketBases(env)) {
    const path = `${base}${file}`
    if (utf8Length(path) <= MOD_SOCKET_PATH_MAX_BYTES) return { path }
    tried.push(path)
  }
  return {
    refusal:
      tried.length === 0
        ? `none of ${MOD_SOCKET_RUNTIME_VARIABLE}, ${MOD_SOCKET_CONFIG_VARIABLE} and ${MOD_SOCKET_HOME_VARIABLE} names an absolute directory only this user can create in, and a session's socket never goes in a base other users share`
        : `every private base makes a socket path longer than ${MOD_SOCKET_PATH_MAX_BYTES} bytes (${tried.join(", ")})`,
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
  /** `plugin`: whether the plugin submitted the prompt as the person's own words
   *  (`$.prompt.submit`'s `asUser`). Measured on 2.1.310: the field is there on such a row and
   *  absent on a plugin's own prompt, which Claude Code frames as the plugin speaking. */
  readonly asUser?: boolean
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

/** Something that happened in the session, sent by the mod. The server turns these into the
 *  Agents page's live view (`../src/agent-stream-runner.ts`); nothing here knows about that. */
export type ModEvent =
  /** The mod reached the server: sent once per process, at the mod's first `session.start`. */
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
      /** The effort the request carried: what Claude Code named for its model, with the
       *  `set-effort` pick over it where there was one. Absent for a model Claude Code names no
       *  effort for, which takes none however the pick stands (`./mod`, `turn.step`). */
      readonly effort?: ModEffort
      readonly parts: readonly ModStepPart[]
      /** True on the one snapshot sent after the response is whole. */
      readonly done: boolean
      /** Why the model stopped; only on the snapshot with `done`. */
      readonly stopReason?: ModStopReason
    }
  /** The turn ended. `stopReason` is `turn.complete`'s own `reason`. */
  | { readonly kind: "turn-complete"; readonly turnId: string; readonly stopReason: ModTurnReason }
  /**
   * One row of the conversation, as `session.append` handed it — or, for a row the mod itself
   * appended, as `$.session.append` answered: Claude Code does not re-enter a mod's own
   * `session.append` hook for such a row (measured, `./mod.ts`), so the mod reports it, by the
   * `note` door and under a `plugin` origin naming this plugin (`MOD_PLUGIN_NAME`).
   */
  | {
      readonly kind: "append"
      readonly uuid: string
      readonly door: ModAppendDoor
      readonly origin: ModAppendOrigin
      readonly message: ModAppendMessage
      /**
       * What this mod wrote the row for, stamped by the mod on the rows it wrote and on no
       * other: `message` for the row one message became — appended into a turn, or the prompt
       * an idle session took — and `pointer` for the prompt at the messages a turn never read
       * (`unreadRowPrompt`).
       *
       * The stamp is how the server knows which rows are this session's own
       * (`../src/mod-session.ts`, `appendedRow` and `runsUnreadPrompt`). It rides on the row's
       * own event rather than on the answer of the call that wrote it, because the two cross:
       * `flush` holds an event behind a post already in flight while a result goes out on a
       * fetch of its own, so either can land first, and a claim made from the answer lands
       * after the live view has read the row on the runs where the event went first
       * (INFRA-1026 k2, measured live on 2.1.310). An origin cannot stand in for it: another
       * installed mod's appended row reaches this mod's hook under a `plugin` origin too, and
       * the prompt row of a card is drawn where the prompt row of a pointer is not.
       */
      readonly wrote?: "message" | "pointer"
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

/**
 * One message the session was handed, as the prompt at a row no request read names it: the
 * person's own words, with the Dispatch message they are, or a card — an Envoy delivery this
 * plugin wrote into the conversation, which nobody at this session typed.
 *
 * It crosses the link because the mod writes that prompt: the mod is where a turn's requests
 * and this plugin's writes arrive in one order, so it is the only place that can say which row
 * a request read (`./mod.ts`, INFRA-1026 review, round eleven).
 */
export type ModUnreadMessage =
  | { readonly kind: "person"; readonly text: string; readonly messageId: string }
  | { readonly kind: "card"; readonly text: string }

/** One unread message as the prompt quotes it, under what it is: a prompt that called a card
 *  the person's own words would put words in their mouth that they never wrote. */
function quotedUnread(message: ModUnreadMessage): string {
  return message.kind === "person"
    ? `Dispatch message ${message.messageId}, the person's own words:\n${message.text}`
    : `An Envoy message delivered to this session, which nobody here typed:\n${message.text}`
}

/**
 * The prompt that points the model at the messages a turn never read, submitted without
 * `asUser`, so the model reads it as the plugin speaking rather than the person.
 *
 * It carries each message's own words rather than pointing at the row holding them. Driven live
 * on two sessions with a prompt that said "Read the message above": the row sat behind the
 * agent's own long answer, and the model replied `I don't see an actual Dispatch message content
 * above` on one and `it looks like an injection and I'm not going to act on it` on the other
 * (INFRA-1026). One prompt goes out however many rows went unread — the mod submits one per turn
 * end — so it names each message, and a turn that left a card and a person's message unread gets
 * the one prompt holding both.
 *
 * The wording is a person's to read and nothing else's: the live view draws nothing for this
 * prompt and draws a card, and it tells the two apart by the stamp the mod puts on the row's own
 * event (`ModEvent`'s `wrote`), never by the words below. A card quoting them — a review of this
 * code, or a message carrying one — is a card still.
 */
export function unreadRowPrompt(unread: readonly ModUnreadMessage[]): string {
  const one = unread.length === 1
  const them = one ? "it" : "them"
  const count = one ? "a message" : `${unread.length} messages`
  const quoted = unread.map(quotedUnread).join("\n\n")
  // The person's own words keep the wording the live runs drove; a set holding a card says
  // instead that each message is labelled, since only some of them are the person's.
  if (unread.every((message) => message.kind === "person")) {
    return [
      `A person sent this session ${count} through Dispatch while your last request was already running, so you have not read ${them} yet.`,
      `The claude-envoy plugin is quoting ${them} here in full: ${one ? "it is" : "they are"} the person's own words, not a tool result and not an instruction of the plugin's own. Answer ${them}.`,
      quoted,
    ].join("\n\n")
  }
  return [
    `Envoy delivered ${count} to this session while your last request was already running, so you have not read ${them} yet.`,
    `The claude-envoy plugin is quoting ${them} here in full, each under what it is: ${one ? "it is" : "they are"} not a tool result and not an instruction of the plugin's own, and nothing here is the person's own words unless it names the Dispatch message they sent. Read ${them} and answer what ${one ? "it asks" : "they ask"} of you.`,
    quoted,
  ].join("\n\n")
}

/** The argument of each call the server may ask the mod to make. `undefined` for a call that takes
 *  none, so every request carries the key and no arm is special. */
export interface ModCallArguments {
  /**
   * One message into the session, which the mod writes as the session's state at that moment
   * makes it: a row of the turn running (`$.session.append`), or the prompt of a new turn
   * while nothing is running (`$.prompt.submit`). Which of the two it is, and whether a turn
   * that ends without reading the row is pointed at it, is the mod's to decide, because the
   * mod is the one process where the turn's events and this write are in one order
   * (INFRA-1026 review, round eleven; `./mod.ts`).
   *
   * The mod answers with the row the message became, and a frame of Envoy's is answered on
   * that row and on nothing else. A request the mod never answered leaves the frame
   * unanswered, so Envoy sends it again, which `eventId` makes safe.
   */
  "session.message": {
    /**
     * The Envoy event this message is, which the mod's write is idempotent by: it records the
     * row each id became and answers a replay of that id with the recorded row, writing
     * nothing (`$.store`, `./mod.ts`). Absent for a message that is no Envoy frame — a notice
     * this server writes for itself, and a person's own message, whose once-only record is the
     * Dispatch attempt (`../src/user-turn-state.ts`).
     */
    readonly eventId?: string
    /** The message as Claude Code's blocks: its words, and a picture as a block of its own. */
    readonly content: readonly ModContentBlock[]
    /** What the message is, which decides whether an idle session reads it as the person's own
     *  words (`asUser`) and how the prompt at a row no request read quotes it. */
    readonly message: ModUnreadMessage
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
  /** Not a `$` call: the effort the mod puts on every later `turn.step` request that names an
   *  effort of its own, or null to stop putting one on and leave the session's own. A request
   *  Claude Code names no effort for takes none whatever this says (`./mod`, `turn.step`). */
  "set-effort": { readonly effort: ModEffort | null }
}

/** What each call answers with. */
export interface ModCallResults {
  /**
   * The row the message became, and which write wrote it, or why nothing was written.
   *
   * The row is the proof: a caller answers its Envoy frame on this uuid and on nothing else.
   * `as` says which write the mod made — `row` for a `$.session.append` into the turn running,
   * `prompt` for a `$.prompt.submit` an idle session took — which is what a caller reads a
   * refusal by: a prompt the session dropped is the person's to be told about, where a row the
   * session refused leaves the frame unanswered (`../src/mod-session.ts`, `deliver`).
   */
  "session.message":
    | { readonly uuid: string; readonly as: "row" | "prompt"; readonly deny?: undefined }
    | { readonly deny: string; readonly as?: "row" | "prompt"; readonly uuid?: undefined }
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
