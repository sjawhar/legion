import { afterEach, expect, test } from "bun:test";
import type { Server, ServerWebSocket } from "bun";

import { readAgentReplay } from "./agent-replay";

// readAgentReplay runs in the page in dispatch-user-turns.sh's `count`, where Playwright's
// `page.evaluate` sends the page its source alone (`toString()`) and the page evaluates that. These
// rows run it the same way: its source evaluated in Bun's global scope, where nothing of this
// module or agent-replay.ts is reachable, so a helper it names from outside the function fails a
// row here as it would fail in the page. They run it against a stand-in for Dispatch's live socket
// that answers the agent view's subscribe as the script tells it to. Its two waits are its own
// real timers, read against real socket traffic, so these rows give it short ones (`emptyAfterMs`
// 300 ms, and a 1 s timeout where the timeout is the point) rather than faking a clock the socket's
// delivery does not run on.

const SESSION = "01a0e0c1-0000-7000-8000-00000000beef";
const SUB = "dispatch-user-turns";
const servers: Server<undefined>[] = [];

afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

const responder = (responding: boolean) =>
  JSON.stringify({ data: { responding, v: 1 }, event: "responder", sub: SUB, type: "agent" });
const replay = { frames: [{ kind: "message", seq: 1, v: 1 }], session_id: SESSION, v: 1 };
const replayFrame = JSON.stringify({ data: replay, event: "replay", sub: SUB, type: "agent" });

/** A Dispatch whose live socket answers each subscribe with `answer`, recording every frame. */
function fakeDispatch(answer: (socket: ServerWebSocket<undefined>) => void): {
  readonly url: string;
  readonly received: unknown[];
} {
  const received: unknown[] = [];
  const server = Bun.serve({
    fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live") {
        return new Response("not found", { status: 404 });
      }
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket, raw) {
        received.push(JSON.parse(String(raw)));
        answer(socket);
      },
    },
  });
  servers.push(server);
  return { received, url: `ws://127.0.0.1:${server.port}/ws/live` };
}

/** How long `promise` takes to settle, and its value or its error's message. */
async function timed(
  promise: Promise<unknown>
): Promise<{ readonly ms: number; readonly value?: unknown; readonly error?: string }> {
  const started = Date.now();
  try {
    const value = await promise;
    return { ms: Date.now() - started, value };
  } catch (error) {
    return {
      error: error instanceof Error ? error.message : String(error),
      ms: Date.now() - started,
    };
  }
}

/** readAgentReplay as `page.evaluate` runs it: its source, evaluated in the global scope, where
 *  nothing of this module or of agent-replay.ts is reachable. Evaluating that source is the point
 *  of these rows, and the source is this repository's own, not a value from anywhere else. */
// biome-ignore lint/security/noGlobalEval: these rows exist to run the function's own source as the page does
const evaluateInGlobalScope = eval;
const inPage = evaluateInGlobalScope(`(${readAgentReplay.toString()})`) as typeof readAgentReplay;

const read = (url: string, timeoutMs = 10_000) =>
  inPage({ emptyAfterMs: 300, sessionId: SESSION, timeoutMs, url });

test("a responding session's replay is the answer", async () => {
  const dispatch = fakeDispatch((socket) => {
    socket.send(responder(true));
    socket.send(replayFrame);
  });
  expect(await read(dispatch.url)).toEqual(replay);
  expect(dispatch.received).toEqual([
    { sub: SUB, type: "subscribe", view: { kind: "agent", session_id: SESSION } },
  ]);
});

test("a responding session with no replay ends at once, saying its replay is empty", async () => {
  const dispatch = fakeDispatch((socket) => socket.send(responder(true)));
  const result = await timed(read(dispatch.url));
  expect(result.error).toBe(
    "the session's replay is empty: Dispatch said the session is responding and sent no replay"
  );
  expect(result.ms).toBeLessThan(5_000);
});

test("a refused agent view ends at once, naming Dispatch's refusal", async () => {
  const dispatch = fakeDispatch((socket) =>
    socket.send(
      JSON.stringify({
        code: "HUMAN_ONLY",
        message: "only users may watch an agent's conversation",
        sub: SUB,
        type: "error",
      })
    )
  );
  const result = await timed(read(dispatch.url));
  expect(result.error).toBe(
    "Dispatch refused the agent view: HUMAN_ONLY: only users may watch an agent's conversation"
  );
  expect(result.ms).toBeLessThan(5_000);
});

test("a view Dispatch ends before its replay says so", async () => {
  const dispatch = fakeDispatch((socket) =>
    socket.send(JSON.stringify({ reason: "dropped", sub: SUB, type: "ended" }))
  );
  const result = await timed(read(dispatch.url));
  expect(result.error).toBe("Dispatch ended the agent view before its replay (dropped)");
  expect(result.ms).toBeLessThan(5_000);
});

test("a session that answers a re-arm after not responding still gives its replay", async () => {
  const dispatch = fakeDispatch((socket) => {
    socket.send(responder(false));
    // Dispatch re-arms the session and asks again; this one answers the second time.
    socket.send(responder(true));
    socket.send(replayFrame);
  });
  expect(await read(dispatch.url)).toEqual(replay);
});

test("a session that never answers ends at the timeout, saying it is not responding", async () => {
  const dispatch = fakeDispatch((socket) => socket.send(responder(false)));
  const result = await timed(read(dispatch.url, 1_000));
  expect(result.error).toBe(
    "the session did not answer Dispatch's replay request within 1s (responding: false), so the agent view has no replay"
  );
});

test("a socket closed before the replay says so", async () => {
  const dispatch = fakeDispatch((socket) => socket.close(1011, "internal error"));
  const result = await timed(read(dispatch.url));
  expect(result.error).toBe(
    "the live socket closed before the session's replay (1011 internal error)"
  );
});
