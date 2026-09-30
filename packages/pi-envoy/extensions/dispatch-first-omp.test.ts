import { afterEach, expect, test } from "bun:test";
import { DISPATCH_FIRST_MARKER } from "@legion/envoy-client/dispatch-first";
import {
  type Cleanup,
  messageStream,
  ompRoot,
  type Request,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "./test-omp-harness";

// The dispatch-first skill's insertion on the real Oh My Pi (src/dispatch-first.ts): Oh My Pi keeps
// nothing a `context` handler returns, so what reaches the model can only be read off the requests
// the binary sends. LEGION_TEST_OMP names the binary, as in legion-phase-stall-omp.test.ts: the
// fork pin CI's pi-envoy job installs. A run without one skips, except on GitHub Actions.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

/** One thing the session is told over RPC, in order: a user prompt, or a manual compaction. */
type Step = { readonly prompt: string } | { readonly compact: true };

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** Whether a Messages request is one of a compaction's summary calls, sent under their own system prompt. */
function isSummarization(request: Request): boolean {
  return JSON.stringify(request.body.system ?? null).includes(
    "Summarize user–AI coding-assistant conversations"
  );
}

/** How many times a Messages request carries the injected skill. */
function markers(request: Request): number {
  return JSON.stringify(request.body).split(DISPATCH_FIRST_MARKER).length - 1;
}

/**
 * Runs an ordinary session (the Envoy extension from this checkout, no Legion environment) through
 * `steps` on the real Oh My Pi, against one stand-in that is the model gateway, the Envoy listener
 * and, with `dispatch`, Dispatch. Each prompt is answered `Answer N.`, each compaction summary call
 * with `summary`. Returns the conversation's Messages requests and the summary calls, in order.
 */
async function runSession(
  binary: string,
  steps: readonly Step[],
  options: { readonly dispatch: boolean; readonly summary?: string }
): Promise<{ readonly turns: Request[]; readonly summaries: Request[] }> {
  const { home, workspace, sessions } = await ompRoot("dispatch-first-omp-", cleanup);

  let answered = 0;
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const summary = isSummarization({ path: url.pathname, body });
      if (!summary) answered += 1;
      const reply = summary
        ? (options.summary ?? "The conversation so far.")
        : `Answer ${answered}.`;
      return new Response(
        messageStream([{ type: "text", text: reply }], `msg_${requests.length}`),
        {
          headers: { "content-type": "text/event-stream" },
        }
      );
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    // An unavailable open-asks read arms no run-end self-check, so every Messages request is a
    // turn of the conversation or a compaction's summary call.
    if (url.pathname === "/api/v1/asks/open") return new Response("unavailable", { status: 503 });
    // The Envoy listener: registration and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: "dispatch-first-machine",
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base, [
    "compaction:",
    // Keep only the newest turn, so a short conversation still has something to summarize.
    "  keepRecentTokens: 1",
    "  methodOrder: [soft]",
  ]);

  // A prompt has settled at its terminal agent_end, a compaction at its RPC response.
  let settle: ((frame: object) => boolean) | undefined;
  let settled = Promise.withResolvers<void>();
  const rpc = spawnRpc(
    binary,
    {
      extensions: ["envoy.ts"],
      home,
      workspace,
      sessions,
      env: {
        ENVOY_URL: base,
        ...(options.dispatch ? { DISPATCH_URL: base, DISPATCH_TOKEN: "dispatch-first-token" } : {}),
      },
      onFrame: (frame) => {
        if (settle?.(frame)) {
          settle = undefined;
          settled.resolve();
        }
      },
    },
    cleanup
  );
  for (const step of steps) {
    settled = Promise.withResolvers<void>();
    let compactError: unknown;
    settle =
      "compact" in step
        ? (frame) => {
            if (!("type" in frame && frame.type === "response")) return false;
            if (!("command" in frame && frame.command === "compact")) return false;
            if (!("success" in frame && frame.success === true)) {
              compactError = "error" in frame ? frame.error : "no error given";
            }
            return true;
          }
        : (frame) =>
            "type" in frame &&
            frame.type === "agent_end" &&
            !("isTerminal" in frame && frame.isTerminal === false);
    rpc.send("compact" in step ? { type: "compact" } : { type: "prompt", message: step.prompt });
    await Promise.race([settled.promise, rpc.closed]);
    if (compactError !== undefined) throw new Error(`omp refused the compaction: ${compactError}`);
  }
  await rpc.end();

  const messages = requests.filter((request) => request.path === "/anthropic/v1/messages");
  return {
    turns: messages.filter((request) => !isSummarization(request)),
    summaries: messages.filter(isSummarization),
  };
}

// A `context` insertion lives only in the one request it was made for, so a skill inserted once
// would reach the first turn and be gone from the second. The compaction case is the other way
// it could drop out: the summary replaces the history the skill sat at the head of.
test.skipIf(omp === undefined && !onActions)(
  "a session with Dispatch carries the dispatch-first skill once in every turn, after a compaction too",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const summary = "SUMMARY-OF-THE-FIRST-TWO-TURNS";
    const session = await runSession(
      omp,
      [
        { prompt: "First question." },
        { prompt: "Second question." },
        { compact: true },
        { prompt: "Third question." },
      ],
      { dispatch: true, summary }
    );

    const [first, second, third, ...rest] = session.turns;
    expect(rest).toEqual([]);
    expect(session.summaries.length).toBeGreaterThan(0);
    for (const turn of [first, second, third]) expect(markers(turn as Request)).toBe(1);
    // The third turn is sent over the compacted history: the summary, then the skill at the head
    // of what follows it, then the new question.
    const third_ = JSON.stringify((third as Request).body.messages);
    expect(third_).not.toContain("First question.");
    expect(third_.indexOf(summary)).toBeGreaterThan(-1);
    expect(third_.indexOf(summary)).toBeLessThan(third_.indexOf(DISPATCH_FIRST_MARKER));
    expect(third_.indexOf(DISPATCH_FIRST_MARKER)).toBeLessThan(third_.indexOf("Third question."));
  },
  180_000
);

test.skipIf(omp === undefined && !onActions)(
  "a session without Dispatch configured carries no dispatch-first skill",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const session = await runSession(omp, [{ prompt: "First question." }], { dispatch: false });

    expect(session.turns).toHaveLength(1);
    expect(markers(session.turns[0] as Request)).toBe(0);
  },
  120_000
);
