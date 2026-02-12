/**
 * Stop-continuation-guard hook.
 *
 * Tracks sessions where the user explicitly stopped execution and prevents
 * automatic continuation (boulder, todo-continuation-enforcer, ralph-loop)
 * from resuming in those sessions.
 *
 * Currently a placeholder — the continuation mechanisms (boulder-state,
 * todo-continuation-enforcer) are Tier 2 roadmap items. This guard is
 * wired now so it's ready when they land.
 *
 * Integration points (from oh-my-opencode):
 *   - tool.execute.before: calls stop() when /stop-continuation is invoked
 *   - chat.message: clears stop state on new user message
 *   - event (session.deleted): cleans up stopped set
 *   - event (session.idle): continuation hooks check isStopped() before injecting
 */

export interface StopContinuationGuard {
  event: (input: { event: { type: string; properties?: unknown } }) => Promise<void>;
  "chat.message": (input: { sessionID?: string }) => Promise<void>;
  stop: (sessionID: string) => void;
  isStopped: (sessionID: string) => boolean;
  clear: (sessionID: string) => void;
}

export function createStopContinuationGuardHook(): StopContinuationGuard {
  const stoppedSessions = new Set<string>();

  const stop = (sessionID: string): void => {
    stoppedSessions.add(sessionID);
  };

  const isStopped = (sessionID: string): boolean => {
    return stoppedSessions.has(sessionID);
  };

  const clear = (sessionID: string): void => {
    stoppedSessions.delete(sessionID);
  };

  const event = async ({
    event,
  }: {
    event: { type: string; properties?: unknown };
  }): Promise<void> => {
    const props = event.properties as Record<string, unknown> | undefined;

    if (event.type === "session.deleted") {
      const sessionInfo = props?.info as { id?: string } | undefined;
      if (sessionInfo?.id) {
        clear(sessionInfo.id);
      }
    }
  };

  const chatMessage = async ({ sessionID }: { sessionID?: string }): Promise<void> => {
    if (sessionID && stoppedSessions.has(sessionID)) {
      clear(sessionID);
    }
  };

  return {
    event,
    "chat.message": chatMessage,
    stop,
    isStopped,
    clear,
  };
}
