import type { ReactNode } from "react";

import { apiErrorMessage } from "../../api/client";
import { RefusableButton } from "../../components/RefusableButton";
import {
  dangerText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { SendStoppedError } from "../writes/keyed-write";
import { StopTrying } from "../writes/StopTrying";
import type { HeldSend } from "./held-sends";
import { KEYED_SEND_LATE_TEXT } from "./send-request";
import type { SessionSends } from "./useSessionSends";

/**
 * One message of the viewer's to a session that Dispatch has not yet made, as its own turn: the
 * text named as theirs - `Your message: “…”`, at a glance and to a screen reader, since it is not
 * yet the session's conversation - and where it stands: waiting for the message before it, never
 * sent, with Discard; sending; still being tried past its deadline with Stop trying; or refused
 * with why, Retry and Discard. The session's live view and its Agents row both show it, and
 * either's controls act on the same write (`useSessionSends`). It leaves once Dispatch makes it,
 * and the stored message takes its place.
 */
export function PendingSendTurn({
  held,
  sends,
}: {
  held: HeldSend;
  sends: Pick<SessionSends, "discard" | "retry" | "stop">;
}): ReactNode {
  return (
    <div
      className="mt-4 flex flex-col items-end gap-1"
      data-message-id={`pending:${held.id}`}
      data-testid="agent-send-held"
    >
      <div
        className={`max-w-[80%] rounded-xl px-3 py-2 text-sm opacity-80 ${surfaceMutedBg} ${textPrimaryOnCanvas}`}
      >
        <p className="whitespace-pre-wrap break-words">Your message: “{held.draft.draft.body}”</p>
      </div>
      <div className="flex max-w-[80%] flex-wrap items-center justify-end gap-x-3 gap-y-1">
        {held.status === "refused" ? (
          <>
            <p className={`text-sm ${dangerText}`} data-testid="agent-send-error">
              {held.refusal instanceof SendStoppedError
                ? held.refusal.message
                : `Could not send: ${apiErrorMessage(held.refusal, "could not reach the session")}`}
            </p>
            <RefusableButton
              look="secondary-compact"
              onPress={() => sends.retry(held)}
              refusal={null}
            >
              Retry
            </RefusableButton>
            <RefusableButton
              look="secondary-compact"
              onPress={() => sends.discard(held)}
              refusal={null}
            >
              Discard
            </RefusableButton>
          </>
        ) : held.status === "waiting" ? (
          <>
            <p className={`text-xs ${textMutedOnCanvas}`} role="status">
              Waiting to send
            </p>
            <RefusableButton
              look="secondary-compact"
              onPress={() => sends.discard(held)}
              refusal={null}
            >
              Discard
            </RefusableButton>
          </>
        ) : (
          <>
            <p className={`text-xs ${textMutedOnCanvas}`} role="status">
              {held.status === "late" ? KEYED_SEND_LATE_TEXT : "Sending…"}
            </p>
            {held.status === "late" ? (
              <StopTrying onStop={() => sends.stop(held)} stopping={held.stopping} />
            ) : null}
          </>
        )}
      </div>
    </div>
  );
}
