// The one read of a session's replay that dispatch-user-turns.ts makes through Dispatch's live
// socket. It runs in the page (`page.evaluate`), as the page's own person, so it is self-contained:
// it imports nothing and reads nothing of this module's scope. agent-replay.test.ts runs it in Bun
// against a stand-in for Dispatch.

/** Where `readAgentReplay` subscribes, and how long it waits. */
export interface AgentReplayRead {
  /** Dispatch's live socket, `ws://` or `wss://` `<host>/ws/live`. */
  readonly url: string;
  /** The session whose agent view is read. */
  readonly sessionId: string;
  /** How long a replay may still follow Dispatch's verdict that the session is responding.
   *  Dispatch sends the replay right after that verdict, and sends none when the session's history
   *  is empty, so a replay that has not come by then is not coming. */
  readonly emptyAfterMs: number;
  /** How long to wait for the replay at all: a session that did not answer Dispatch's replay
   *  request may answer one of Dispatch's re-arms within it. */
  readonly timeoutMs: number;
}

/**
 * Subscribes `read.sessionId`'s agent view and resolves with the session's replay as Dispatch
 * relays it. Otherwise it rejects with an error that says which way it ended: Dispatch refused the
 * view, or ended it, the session answered with an empty replay, the session never answered
 * Dispatch's replay request, Dispatch sent nothing for the view, or the socket closed first.
 */
export function readAgentReplay(read: AgentReplayRead): Promise<unknown> {
  const result = Promise.withResolvers<unknown>();
  const sub = "dispatch-user-turns";
  const seconds = read.timeoutMs / 1_000;
  const socket = new WebSocket(read.url);
  // Dispatch's last verdict on whether the session answers on its control subject, null before
  // its first.
  let responding: boolean | null = null;
  let settled = false;
  // Cancels the wait for a replay after a responding verdict, while one runs.
  let cancelEmpty: (() => void) | undefined;
  const settle = (outcome: { readonly replay: unknown } | Error) => {
    if (settled) return;
    settled = true;
    clearTimeout(timeout);
    cancelEmpty?.();
    socket.close(1000);
    if (outcome instanceof Error) result.reject(outcome);
    else result.resolve(outcome.replay);
  };
  const timeout = setTimeout(() => {
    settle(
      new Error(
        responding === null
          ? `Dispatch sent nothing for the agent view within ${seconds}s`
          : `the session did not answer Dispatch's replay request within ${seconds}s (responding: false), so the agent view has no replay`
      )
    );
  }, read.timeoutMs);

  socket.addEventListener("open", () => {
    socket.send(
      JSON.stringify({
        sub,
        type: "subscribe",
        view: { kind: "agent", session_id: read.sessionId },
      })
    );
  });
  socket.addEventListener("message", (message) => {
    let frame: unknown;
    try {
      frame = JSON.parse(String(message.data));
    } catch {
      return;
    }
    if (typeof frame !== "object" || frame === null || !("type" in frame)) return;
    // A refusal names the subscription it refuses, or none for a frame Dispatch could not read.
    if (frame.type === "error" && (!("sub" in frame) || frame.sub === sub)) {
      const code = "code" in frame ? String(frame.code) : "";
      const reason = "message" in frame ? String(frame.message) : "";
      settle(new Error(`Dispatch refused the agent view: ${code}: ${reason}`));
      return;
    }
    if (!("sub" in frame) || frame.sub !== sub) return;
    if (frame.type === "ended") {
      const reason = "reason" in frame ? String(frame.reason) : "";
      settle(new Error(`Dispatch ended the agent view before its replay (${reason})`));
      return;
    }
    if (frame.type !== "agent" || !("event" in frame) || !("data" in frame)) return;
    if (frame.event === "responder") {
      const data = frame.data;
      responding =
        typeof data === "object" &&
        data !== null &&
        "responding" in data &&
        data.responding === true;
      cancelEmpty?.();
      cancelEmpty = undefined;
      if (responding) {
        const empty = setTimeout(() => {
          settle(
            new Error(
              "the session's replay is empty: Dispatch said the session is responding and sent no replay"
            )
          );
        }, read.emptyAfterMs);
        cancelEmpty = () => clearTimeout(empty);
      }
      return;
    }
    if (frame.event === "replay") settle({ replay: frame.data });
  });
  socket.addEventListener("close", (event) => {
    settle(
      new Error(
        `the live socket closed before the session's replay (${event.code} ${event.reason})`
      )
    );
  });
  return result.promise;
}
