/**
 * The mod's half of the link (`../hooks/mod.ts`), for a test that drives a whole channel-server
 * process: it polls the socket the server bound, answers the calls the server makes, and sends
 * the events a real session would. What the real mod adds beyond this is Claude Code itself.
 */
import {
  MOD_ROUTE_EVENTS,
  MOD_ROUTE_REQUESTS,
  MOD_ROUTE_RESULTS,
  MOD_URL_BASE,
  type ModCallArguments,
  type ModCallName,
  type ModCallResults,
  type ModEvent,
  type ModRequest,
  type ModResultValue,
} from "../hooks/mod-protocol"

/** One call the server asked the mod to make. */
export interface ModCall {
  readonly call: ModCallName
  readonly args: ModCallArguments[ModCallName]
}

/** What this mod answers each call with; a call with no answer here gets the default below. */
export type ModAnswers = {
  [N in ModCallName]?: (args: ModCallArguments[N]) => ModCallResults[N] | Promise<ModCallResults[N]>
}

export interface FakeMod {
  /** Every call the server made, in order. */
  readonly calls: readonly ModCall[]
  /** The calls of one name, in order. */
  madeOf<N extends ModCallName>(call: N): readonly ModCallArguments[N][]
  /** Sends one event, as the mod's hooks do. */
  send(event: ModEvent): Promise<void>
  /** Stops polling; the server's link stays up. */
  stop(): Promise<void>
}

/** A row id for each prompt Claude Code takes, which is a row of its own. */
let prompts = 0

/**
 * The row Claude Code writes for a prompt the mod submitted, as 2.1.310 writes it: by the
 * `prompt` door, under the plugin's origin, carrying `asUser` where the submit did and Claude
 * Code's own framing in front of the text where it did not
 * (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:52-53`). The mod stamps the row
 * it wrote, which is what the server claims it by (`ModEvent`'s `wrote`), and answers the
 * server with its uuid: the row is the proof the session has the message.
 */
function promptRow(
  args: ModCallArguments["session.message"],
): Extract<ModEvent, { kind: "append" }> {
  prompts += 1
  const asUser = args.message.kind === "person"
  const text = asUser
    ? args.message.text
    : `The claude-envoy plugin sent a message:\n${args.message.text}`
  return {
    door: "prompt",
    kind: "append",
    message: { content: [{ text, type: "text" }], role: "user", type: "user" },
    origin: { kind: "plugin", name: "claude-envoy", ...(asUser ? { asUser: true } : {}) },
    uuid: `prompt-row-${prompts}`,
    wrote: "message",
  }
}

export async function startFakeMod(
  socketPath: string,
  options: {
    readonly sessionId: string
    readonly answers?: ModAnswers
    /** Sent as the first event, as a real mod's `session.start` does; true by default. */
    readonly hello?: boolean
  },
): Promise<FakeMod> {
  const calls: ModCall[] = []
  let stopped = false

  const post = async (route: string, body: unknown): Promise<unknown> => {
    const answer = await fetch(`${MOD_URL_BASE}${route}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
      unix: socketPath,
    })
    return answer.json()
  }

  const answerOf = async (request: ModRequest): Promise<ModResultValue> => {
    const answers = options.answers ?? {}
    switch (request.call) {
      case "session.message": {
        const own = answers["session.message"]
        if (own !== undefined) return await own(request.args)
        // What the real mod does with a session that has no turn running: the message is the
        // prompt, Claude Code writes its row before the submit resolves, and the mod answers
        // the server with that row (`../hooks/mod.ts`, `writeMessage`).
        const row = promptRow(request.args)
        await send(row)
        return { uuid: row.uuid, as: "prompt" }
      }
      case "command.run": {
        const own = answers["command.run"]
        return own === undefined ? { text: "" } : await own(request.args)
      }
      case "model.fork": {
        const own = answers["model.fork"]
        return own === undefined ? { isAnswered: true, text: "" } : await own(request.args)
      }
      case "config.set": {
        const own = answers["config.set"]
        return own === undefined ? { value: request.args.value } : await own(request.args)
      }
      case "session.model": {
        const own = answers["session.model"]
        return own === undefined ? "claude-sonnet-4-5" : await own(request.args)
      }
      case "config.list": {
        const own = answers["config.list"]
        return own === undefined ? [] : await own(request.args)
      }
      case "command.list": {
        const own = answers["command.list"]
        return own === undefined ? [] : await own(request.args)
      }
      default: {
        const own = answers["set-effort"]
        return own === undefined ? { effort: request.args.effort } : await own(request.args)
      }
    }
  }

  const poll = async (): Promise<void> => {
    while (!stopped) {
      let requests: readonly ModRequest[] = []
      try {
        const body = (await post(MOD_ROUTE_REQUESTS, {})) as { requests?: readonly ModRequest[] }
        requests = body.requests ?? []
      } catch {
        // The server went away; a real mod backs off and gives up, and so does this.
        return
      }
      for (const request of requests) {
        calls.push({ call: request.call, args: request.args })
        try {
          const value = await answerOf(request)
          await post(MOD_ROUTE_RESULTS, { results: [{ id: request.id, ok: true, value }] })
        } catch (error) {
          await post(MOD_ROUTE_RESULTS, {
            results: [{ id: request.id, ok: false, error: String(error) }],
          }).catch(() => undefined)
        }
      }
    }
  }

  const send = async (event: ModEvent): Promise<void> => {
    await post(MOD_ROUTE_EVENTS, { events: [event] })
  }

  // The server binds its socket while its MCP transport starts, so a mod started beside it
  // probes until the socket answers, as the real mod does.
  const deadline = Date.now() + 20_000
  for (;;) {
    try {
      if (options.hello !== false) {
        await send({
          kind: "hello",
          sessionId: options.sessionId,
          claudeVersion: "2.1.310",
          interactive: true,
        })
      }
      break
    } catch (error) {
      if (Date.now() > deadline) throw error
      await Bun.sleep(50)
    }
  }
  void poll()

  return {
    calls,
    madeOf: <N extends ModCallName>(call: N): readonly ModCallArguments[N][] =>
      calls.filter((made) => made.call === call).map((made) => made.args as ModCallArguments[N]),
    send,
    async stop() {
      stopped = true
    },
  }
}
