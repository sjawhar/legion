import { type ReactNode, useState } from "react";

import type { Subscriber } from "../../api/types";
import { textSecondaryOnSurface } from "../../theme/classes";
import { sessionLabel } from "../refs/actor";
import { LiveDot, sessionChipClassName, sessionRemoveButtonClassName } from "../refs/SessionChip";
import { Timestamp } from "../refs/Timestamp";
import { UnsubscribeDialog } from "./UnsubscribeDialog";

export interface SubscribedAgentsProps {
  onUnsubscribe: (sessionId: string) => void;
  /** The issue key, or `<project>/<slug>` for a project document — named in the confirm
   * dialog's message and nowhere else, so callers pass a precomputed string rather than an
   * issue/document union this component would have to branch on. */
  ownerLabel: string;
  subscribers: readonly Subscriber[];
}

/**
 * The "Subscribed agents" header section shared by an issue's header and a project
 * document's page: sessions whose persisted Envoy interests include this owner's topic
 * family, each shown with a live/offline dot, its title (or shortened session id), how
 * long ago it was last seen, and an Unsubscribe control that confirms before telling the
 * session it was removed.
 */
export function SubscribedAgents({
  onUnsubscribe,
  ownerLabel,
  subscribers,
}: SubscribedAgentsProps): ReactNode {
  const [confirming, setConfirming] = useState<Subscriber | undefined>(undefined);
  const confirmingLabel =
    confirming === undefined ? "" : sessionLabel(confirming.session_id, confirming.title);

  if (subscribers.length === 0) {
    return null;
  }

  return (
    <section aria-label="Subscribed agents" className="mt-2">
      <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Subscribed agents</h2>
      <ul className="mt-2 flex flex-wrap gap-2">
        {subscribers.map((subscriber) => {
          const label = sessionLabel(subscriber.session_id, subscriber.title);
          return (
            <li className={sessionChipClassName} key={subscriber.session_id}>
              <LiveDot live={subscriber.live} />
              <span title={subscriber.session_id}>{label}</span>
              <Timestamp at={new Date(subscriber.last_seen).toISOString()} />
              {subscriber.removable ? null : (
                <span className="text-xs italic">via {subscriber.via}</span>
              )}
              <button
                className={sessionRemoveButtonClassName}
                disabled={!subscriber.removable}
                onClick={() => setConfirming(subscriber)}
                title={
                  subscriber.removable
                    ? undefined
                    : `Subscribed only via ${subscriber.via}, which also covers other issues and documents — cannot unsubscribe just this one`
                }
                type="button"
              >
                Unsubscribe
              </button>
            </li>
          );
        })}
      </ul>
      {confirming === undefined ? null : (
        <UnsubscribeDialog
          message={`Unsubscribe ${confirmingLabel} from ${ownerLabel}? They will be told.`}
          onCancel={() => setConfirming(undefined)}
          onConfirm={() => {
            onUnsubscribe(confirming.session_id);
            setConfirming(undefined);
          }}
        />
      )}
    </section>
  );
}
