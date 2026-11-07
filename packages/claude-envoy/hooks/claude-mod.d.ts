/**
 * The part of Claude Code's mod API that `./mod.ts` uses, hand-written from the declarations
 * Claude Code 2.1.310 ships (`@anthropic-ai/claude-code`'s `index.d.ts`, 17k lines), measured on
 * that release.
 *
 * Hand-written on purpose: taking the package as a devDependency for those 17k lines would move
 * `bun.lock`, and `bun.lock` is an input of claude-envoy's build (`scripts/build-inputs.txt`), so
 * every change to it rebuilds and re-commits `dist/` and `legion/`. The mod ships from source and
 * imports nothing of the package at run time, so nothing but type-checking needs it.
 *
 * What is here is what the mod uses, copied faithfully, with two reductions, both safe because
 * the mod only reads or relays the values:
 *   - a type the mod receives and passes on whole (`TurnStepResult`) declares the fields the mod
 *     reads, not every field Claude Code puts on it;
 *   - a string union the mod never branches on (`ModelForkResult`'s `reason`) is declared `string`.
 * `on(...)` really answers a `Registration` whose `catch` sets a handler; the mod sets none, so
 * the overloads here answer `void`.
 *
 * When this plugin is built against a newer Claude Code, check this file against that release's
 * declarations: nothing else does.
 */
declare module "claude-code" {
  /** One block of a message in Messages API form (`ApiContentBlock`). */
  export type ApiContentBlock = {
    type: string
    [field: string]: unknown
  }

  /** The version of the engine the session runs on (`SessionVersion`). */
  export type SessionVersion = {
    /** The engine's full version, as `claude --version` prints it. */
    version: string
    /** The release the version is built from; absent when it is not spelled as one. */
    base?: string
    /** When the binary was built, ISO 8601; absent in a run from source that stamps none. */
    builtAt?: string
  }

  /** The input of `session.start`: the session as `$.session` reads it at that moment. */
  export type SessionStartInput = {
    /** The directory the session runs in, absolute. */
    readonly cwd: string
    /** Whether a person is at the prompt: true under the REPL, false for `-p` or the SDK. */
    readonly isInteractive: boolean
  }

  /** What `session.start`'s `next(e)` resolves to; a hook's own value changes nothing. */
  export type SessionStartResult = {
    cwd: string
  }

  /** Why the session ended, the classic SessionEnd hook's own word. */
  export type SessionEndReason = "clear" | "resume" | "logout" | "prompt_input_exit" | "other"

  /** The input of `session.end`. */
  export type SessionEndInput = {
    readonly reason: SessionEndReason
    /** The ending session's id; after a `/clear` or a resume the process goes on under another. */
    readonly sessionId: string
  }

  /** What `session.end`'s `next(e)` resolves to. */
  export type SessionEndResult = {
    sessionId: string
  }

  /** The input of `turn.start`: the prompt a model turn begins with. */
  export type TurnStartInput = {
    readonly text: string
    /** The turn's id, minted here; every `turn.step` and the `turn.complete` carry it. */
    readonly turnId: string
    /** The loop the turn runs in: a subagent's id; absent on the main loop. */
    readonly agentId?: string
  }

  /** What `turn.start`'s `next(e)` resolves to. */
  export type TurnStartResult = {
    turnId: string
  }

  /** Why a turn ended. */
  export type TurnCompleteReason = "answer" | "aborted" | "refusal" | "error"

  /** The input of `turn.complete`: the assistant's final message of a turn. */
  export type TurnCompleteInput = {
    readonly answer: string
    readonly durationMs: number
    readonly isAborted: boolean
    readonly turnId: string
    readonly reason: TurnCompleteReason
    /** The loop the turn ran in: a subagent's id; absent on the main loop. */
    readonly agentId?: string
  }

  /** What `turn.complete`'s `next(e)` resolves to. */
  export type TurnCompleteResult = {
    text: string
  }

  /** How hard a request asks the model to think; absent for a model without effort. */
  export type TurnEffort = "low" | "medium" | "high" | "xhigh" | "max" | number

  /** The input of `turn.step`: one model request inside a turn. A hook rewrites `model` or
   *  `effort` going down; the rest is pinned. */
  export type TurnStepInput = {
    readonly turnId: string
    /** The step's position in the turn, from 0. */
    readonly index: number
    readonly model: string
    readonly effort?: TurnEffort
    readonly messageCount: number
    /** The loop the request is made in: a subagent's id; absent on main. */
    readonly agentId?: string
  }

  /** The engine's handle on its own streamed item; opaque, one step's only. */
  export type ChunkRef = {
    readonly ref?: number
  }

  /** A piece of the response's visible text, in block `index`. */
  export type TurnStepTextChunk = ChunkRef & {
    readonly kind: "text"
    readonly index: number
    readonly text: string
  }

  /** A piece of the model's thinking, in block `index`. */
  export type TurnStepThinkingChunk = ChunkRef & {
    readonly kind: "thinking"
    readonly index: number
    readonly text: string
  }

  /** The model begins a tool call in block `index`; its arguments follow as `input` chunks. */
  export type TurnStepToolChunk = ChunkRef & {
    readonly kind: "tool"
    readonly index: number
    /** The call's `tool_use_id`. */
    readonly id: string
    /** The tool's name (`Read`, `Bash`, `mcp__server__tool`). */
    readonly name: string
  }

  /** A piece of a tool call's arguments as they arrive: JSON text, partial. */
  export type TurnStepInputChunk = ChunkRef & {
    readonly kind: "input"
    readonly index: number
    readonly json: string
  }

  /** Why the model stopped; null when no response arrived. */
  export type TurnStopReason =
    | "end_turn"
    | "max_tokens"
    | "stop_sequence"
    | "tool_use"
    | "pause_turn"
    | "compaction"
    | "refusal"
    | "model_context_window_exceeded"
    | null

  /** The response is whole: the last chunk of one response. */
  export type TurnStepStopChunk = ChunkRef & {
    readonly kind: "stop"
    readonly stopReason: TurnStopReason
  }

  /** An item of the engine's stream the other chunk kinds do not model; pass it on unread. */
  export type TurnStepEngineChunk = {
    readonly kind: "engine"
    readonly ref: number
  }

  /** One piece of a `turn.step` response as it streams through the chain. */
  export type TurnStepChunk =
    | TurnStepTextChunk
    | TurnStepThinkingChunk
    | TurnStepToolChunk
    | TurnStepInputChunk
    | TurnStepStopChunk
    | TurnStepEngineChunk

  /** What a `turn.step` resolves to, in the fields the mod reads: the rest of the object is
   *  relayed whole and never looked at. */
  export type TurnStepResult = {
    readonly turnId: string
    readonly index: number
    readonly stopReason: TurnStopReason
  }

  /** A stream a hook's `next(e)` answers on a streaming event. */
  export type HookStream<C, R> = AsyncGenerator<C, R> & {
    /** Settles with what beneath returned once the stream has been read to its end. */
    readonly result: Promise<R>
  }

  /** Which door a row came in by; a closed set, pinned on the event. */
  export type SessionAppendDoor =
    | "prompt"
    | "command"
    | "response"
    | "tool-result"
    | "tool-message"
    | "delivery"
    | "attachment"
    | "hook-context"
    | "note"
    | "compaction"
    | "notice"
    | "queue"

  /** One row of a conversation as `session.append` hands it. */
  export type SessionAppendMessage = {
    readonly type: "user" | "assistant" | "attachment" | "system"
    /** An attachment's type or a notice's subtype; absent on user and assistant rows. */
    readonly name?: string
    /** Under which role a request carries the row; absent when none does. */
    readonly role?: "user" | "assistant"
    /** True on a user-side row the person does not see as typed. */
    readonly isMeta?: true
    readonly content: readonly ApiContentBlock[]
  }

  /** Who caused a row: the person, the model, a tool, the engine, a settings hook, a plugin.
   *  Claude Code spells it as a union of one arm per kind, each with the field that kind carries;
   *  the mod relays the kind and those fields, and branches on none. */
  export type SessionAppendOrigin = {
    readonly kind: string
    readonly name?: string
    readonly model?: string
    readonly tool?: string
    readonly event?: string
    readonly server?: string
  }

  /** The input of `session.append`: one row being appended to a conversation of this session. */
  export type SessionAppendInput = {
    readonly message: SessionAppendMessage
    readonly door: SessionAppendDoor
    readonly origin: SessionAppendOrigin
    /** The row's id, the same in the transcript file and on every later read. */
    readonly uuid: string
    /** The loop whose conversation keeps the row: a subagent's id; absent on main. */
    readonly agentId?: string
  }

  /** The row as stored, or the refusal of a plugin's own append. */
  export type SessionAppendResult =
    | { readonly message: SessionAppendMessage; readonly uuid: string; readonly deny?: undefined }
    | { readonly deny: string; readonly message?: undefined; readonly uuid?: undefined }

  /** The argument of `$.session.append`: a user row the model reads, or a system notice. */
  export type SessionAppendArgs = {
    readonly message: {
      readonly type: "user" | "system"
      readonly content: readonly ApiContentBlock[]
    }
    /** The running subagent whose conversation the row joins; absent for main. */
    readonly agentId?: string
  }

  /** The argument of `$.prompt.submit`. */
  export type PromptSubmitArgs = {
    readonly text: string
    /** Submit the text as the person's own words, read without the plugin's frame. */
    readonly asUser?: true
  }

  /** What `$.prompt.submit` resolves to: the prompt that entered, or why it did not. */
  export type PromptSubmitResult =
    | { readonly text: string; readonly drop?: undefined }
    | { readonly drop: string; readonly text?: undefined }

  /** The argument of `$.command.run`. */
  export type CommandRunArgs = {
    readonly command: string
    /** Everything after the name, as the person would type it; left out, `""`. */
    readonly args?: string
  }

  /** What `$.command.run` resolves to. */
  export type CommandRunResult = {
    /** The command's output as a transcript line; absent when it showed nothing as text. */
    readonly text?: string
    /** What the model reads after the output and the person never sees. */
    readonly context?: readonly string[]
  }

  /** Where a command comes from. */
  export type CommandSource = "builtin" | "plugin" | "user" | "mcp"

  /** One slash command as `$.command.list()` answers it. */
  export type CommandInfo = {
    readonly name: string
    readonly description: string
    readonly source: CommandSource
    /** Which plugin added it, when `source` is `plugin` and the engine knows. */
    readonly plugin?: string
  }

  /** A `/config` row's value. */
  export type ConfigValue = boolean | string | number | readonly string[]

  /** How a `/config` row takes its value. */
  export type ConfigKind = "boolean" | "choice" | "text" | "number"

  /** Where a hook's plugin sits among the chain's tiers. */
  export type Tier = "prepend" | "user" | "append" | "builtin" | "core"

  /** Who owns a row or made a call. */
  export type Origin = {
    readonly plugin: string
    readonly tier: Tier
  }

  /** One `/config` row as `$.config.list()` answers it. */
  export type ConfigRow = {
    readonly key: string
    readonly label: string
    readonly description?: string
    readonly kind: ConfigKind
    readonly value: ConfigValue
    /** The values a `choice` row takes, in order; absent otherwise. */
    readonly options?: readonly string[]
    readonly provider: Origin
    /** Whether a trusted source owns the value, so the menu refuses a change. */
    readonly isLocked: boolean
  }

  /** The argument of `$.config.set`. */
  export type ConfigSetArgs = {
    readonly key: string
    readonly value: ConfigValue
  }

  /** What `$.config.set` resolves to: the value once written, or why not. */
  export type ConfigSetResult =
    | { readonly value: ConfigValue; readonly deny?: undefined }
    | { readonly deny: string; readonly value?: undefined }

  /** The argument of `$.model.fork`. */
  export type ModelForkRequest = {
    /** The one user message, appended to the session's own transcript as the main thread last
     *  sent it. */
    readonly prompt: string
  }

  /** What `$.model.fork` resolves to. `reason` is the arm's own word (`api-error`,
   *  `empty-reply`, `aborted`, `nothing-to-fork`), which the mod relays and never branches on. */
  export type ModelForkResult =
    | { readonly isAnswered: true; readonly text: string }
    | { readonly isAnswered: false; readonly reason: string }

  /** Options of `$.http.fetch`. */
  export type HttpInit = {
    /** `GET` (default), `POST`, ... */
    method?: string
    /** Request headers. */
    headers?: Record<string, string>
    /** The request body, as text. */
    body?: string
    /** The absolute path of a Unix domain socket the request goes over instead of TCP (near
     *  100 B at most): one per session, in a private directory. The URL still gives path, query
     *  and Host. No relative path, no NUL. */
    socketPath?: string
  }

  /** What `$.http.fetch` resolves with. */
  export type HttpResponse = {
    readonly status: number
    /** True for a 2xx status. */
    readonly ok: boolean
    /** Response headers, lower-cased names. */
    readonly headers: Record<string, string>
    /** The body, as text. */
    readonly text: string
  }

  /** Options of `$.clock.sleep`. */
  export type SleepOptions = {
    /** Ends the wait early with a rejection. */
    signal?: AbortSignal
  }

  /** The engine as a hook receives it, in the nouns the mod calls. */
  export interface EngineInterface {
    readonly session: {
      /** The session's id, as it stands now: a `/clear` moves it. */
      id: () => Promise<string>
      /** The version of the engine the session runs on. */
      version: () => Promise<SessionVersion>
      /** The session's model, by the id the engine resolved. */
      model: () => Promise<string>
      /** Appends one row to a conversation of this session. */
      append: (args: SessionAppendArgs) => Promise<SessionAppendResult>
    }
    readonly prompt: {
      /** Submits a prompt; resolves once its turn ended. */
      submit: (input: PromptSubmitArgs) => Promise<PromptSubmitResult>
    }
    readonly command: {
      list: () => Promise<CommandInfo[]>
      run: (input: CommandRunArgs) => Promise<CommandRunResult>
    }
    readonly config: {
      list: () => Promise<ConfigRow[]>
      set: (input: ConfigSetArgs) => Promise<ConfigSetResult>
    }
    readonly model: {
      fork: (request: ModelForkRequest) => Promise<ModelForkResult>
    }
    readonly http: {
      /** Fetches `url` through the host; `init.socketPath` sends it over a Unix socket. */
      fetch: (url: string, init?: HttpInit) => Promise<HttpResponse>
    }
    readonly env: {
      /** The variable's value, or undefined when it is unset. `name` must be a string
       *  literal: `claude plugin validate` lists the names a module reads. */
      get: (name: string) => Promise<string | undefined>
    }
    readonly clock: {
      /** Resolves after `ms` milliseconds; rejects at once when `signal` aborts. */
      sleep: (ms: number, options?: SleepOptions) => Promise<void>
    }
    readonly store: {
      /** The value under `key`, or undefined when it is unset. */
      get: (key: string) => Promise<unknown>
      /** Sets `key` to `value`, which must be JSON data: `get` reads back
       *  `JSON.parse(JSON.stringify(value))`. Rejects a function, a cycle, or a store past
       *  4 MiB of JSON text in all. */
      set: (key: string, value: unknown) => Promise<void>
      delete: (key: string) => Promise<void>
      /** Every key set, in insertion order. */
      keys: () => Promise<string[]>
    }
  }

  /** Registers the hooks the mod uses. Claude Code's own `On` is one generic pair of overloads
   *  over every event; these are the events the mod registers, each spelled out. */
  export type On = {
    (
      pattern: "session.start",
      hook: (
        $: EngineInterface,
        e: SessionStartInput,
        next: (e: SessionStartInput) => Promise<SessionStartResult>,
      ) => Promise<SessionStartResult>,
    ): void
    (
      pattern: "session.end",
      hook: (
        $: EngineInterface,
        e: SessionEndInput,
        next: (e: SessionEndInput) => Promise<SessionEndResult>,
      ) => Promise<SessionEndResult>,
    ): void
    (
      pattern: "turn.start",
      hook: (
        $: EngineInterface,
        e: TurnStartInput,
        next: (e: TurnStartInput) => Promise<TurnStartResult>,
      ) => Promise<TurnStartResult>,
    ): void
    (
      pattern: "turn.complete",
      hook: (
        $: EngineInterface,
        e: TurnCompleteInput,
        next: (e: TurnCompleteInput) => Promise<TurnCompleteResult>,
      ) => Promise<TurnCompleteResult>,
    ): void
    /** Claude Code spells the generator's return `TurnStepResult | void`, which lets a hook
     *  return nothing and leave the result beneath standing; `undefined` says the same thing of a
     *  generator and is the spelling this package's lint allows. */
    (
      pattern: "turn.step",
      hook: (
        $: EngineInterface,
        e: TurnStepInput,
        next: (e: TurnStepInput) => HookStream<TurnStepChunk, TurnStepResult>,
      ) => AsyncGenerator<TurnStepChunk, TurnStepResult | undefined>,
    ): void
    (
      pattern: "session.append",
      matcher: { readonly door: SessionAppendDoor },
      hook: SessionAppendHook,
    ): void
  }

  /** The hook `on("session.append", { door }, hook)` takes, named so one hook can serve every
   *  door's registration. */
  export type SessionAppendHook = (
    $: EngineInterface,
    e: SessionAppendInput,
    next: (e: SessionAppendInput) => Promise<SessionAppendResult>,
  ) => Promise<SessionAppendResult>
}
