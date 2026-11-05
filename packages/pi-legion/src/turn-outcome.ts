import { messageFor } from "@legion/envoy-client/errors";
import { type LegionDaemonClient, retryWhileDaemonRestarts } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndEvent } from "@legion/pi-shared/pi-types";
import { classifyProviderTurn, type ProviderTurnOutcome } from "@legion/pi-shared/provider-refusal";

/**
 * The Oh My Pi half of the turn-outcome report (dispatch://INFRA-956). Every turn this pane
 * settles is reported to the daemon over HTTP, under a sequence this plugin counts itself, with
 * the class `@legion/pi-shared/provider-refusal` read from the turn's last assistant message: a
 * content filter is `refused`, with the classifier's category, every other provider failure is
 * `provider-error`, and a turn the provider ended cleanly is `clean`. The daemon treats the first
 * differently from the second — the same session on a weaker model often takes a turn a filter
 * refused, and a 429 or a malformed request may not clear that way — so the two never arrive
 * under one word.
 *
 * The daemon parses no provider text: this report carries the class and a refusal's own category,
 * never the provider's words. A refusal's explanation is itself refused when it is sent back
 * (dispatch://INFRA-956/artifact/cause-evidence-md).
 *
 * The count is this producer's alone. It is not the daemon's turn ordinal and not
 * `supervise.StreamTurnEnd`'s: two counters would drift the moment one of them missed an end, and
 * only one of them can own the number a report is keyed by. It starts at 1 within a claim's
 * launch generation, which a pane holds for its whole life, and is taken before the report is
 * sent, so reports that overlap keep the order their turns ended in whatever order they arrive —
 * the daemon keeps the newest `(generation, sequence)` and a late report never replaces a newer
 * one.
 */

export interface TurnOutcomeLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export interface TurnOutcomeReporterDeps {
  /** The pane's daemon client, built lazily by the entry that owns it. */
  readonly daemon: () => LegionDaemonClient;
  /** The claim the session with that id registered, or undefined: a session before its
   * registration, the controller, a person's own pane, and a `task` subagent, which loads an
   * instance of its own and registers nothing. */
  readonly claim: (sessionID: string) => ClaimCapability | undefined;
  readonly log: TurnOutcomeLog;
}

export interface TurnOutcomeReporter {
  /**
   * Reports the turn that `agent_end` just ended, for the claim the session holds. Never rejects:
   * Oh My Pi does not await an `agent_end` handler, so a rejection here would be an unhandled one,
   * and nothing of the turn waits on the report either way.
   */
  readonly report: (sessionID: string, event: AgentEndEvent) => Promise<void>;
}

/** The word the daemon reads for the class the provider ended the turn with
 * (`claim.TurnClean`, `claim.TurnRefused`, `claim.TurnProviderError`). A content filter is
 * `refused` because the same session takes that turn again on a weaker model; every other
 * provider failure is `provider-error`, which may not clear that way. */
function outcomeWord(kind: ProviderTurnOutcome["kind"]): "clean" | "refused" | "provider-error" {
  switch (kind) {
    case "clean":
      return "clean";
    case "content-filter":
      return "refused";
    default:
      return "provider-error";
  }
}

export function createTurnOutcomeReporter(deps: TurnOutcomeReporterDeps): TurnOutcomeReporter {
  /** The generation the sequence below counts within, and the last number it gave out. */
  let counting: number | undefined;
  let sequence = 0;

  return {
    report: async (sessionID, event) => {
      // The host is about to retry this turn itself, so the turn has not settled: it reports
      // nothing and takes no number, and the report of the turn it ends with carries the class
      // that turn settled on (`src/model-fallback.ts` reads the same flag).
      if (event.willContinue === true) return;
      const claim = deps.claim(sessionID);
      if (claim === undefined) return;
      // Taken before the first await, so two turns can never take the same number nor take them
      // out of the order they ended in. A generation this producer has not counted within starts
      // the count again, as a fresh launch's does.
      if (claim.generation !== counting) {
        counting = claim.generation;
        sequence = 0;
      }
      sequence += 1;
      // The number this report owns: `sequence` moves on as the next turn ends, and the lines
      // below are written after an await.
      const mine = sequence;
      const turn = classifyProviderTurn(
        event.messages?.findLast((message) => message.role === "assistant")
      );
      const request = {
        claimToken: claim.claimToken,
        sessionId: claim.sessionID,
        secret: claim.secret,
        generation: claim.generation,
        sequence: mine,
        outcome: outcomeWord(turn.kind),
        ...(turn.kind === "content-filter" && turn.category !== undefined
          ? { category: turn.category }
          : {}),
      };
      try {
        // A send that fails on nothing but a restarting daemon is retried until it lands: the
        // daemon keeps one report per claim, keyed by `(generation, sequence)`, so a repeat of
        // this report writes the same fact, and a turn that goes unreported is a turn the daemon
        // knows nothing about — a refusal silently dropped is a worker left on the model that
        // refused it. A refusal the daemon itself gives (a 4xx) will not pass on retry: it is
        // warned about once and the turn goes unreported, since nothing this pane can do fixes
        // it.
        await retryWhileDaemonRestarts("claims/turn-outcome", () =>
          deps.daemon().turnOutcome(request)
        );
        deps.log.debug("legion: reported a turn's outcome", {
          issue: claim.issue,
          role: claim.role,
          sequence: mine,
          outcome: request.outcome,
        });
      } catch (error) {
        deps.log.warn("legion: a turn's outcome did not reach the daemon", {
          issue: claim.issue,
          role: claim.role,
          sequence: mine,
          outcome: request.outcome,
          error: messageFor(error),
        });
      }
    },
  };
}
