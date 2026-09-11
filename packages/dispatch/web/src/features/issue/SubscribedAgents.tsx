import { type ReactNode, useState } from "react";

import type { Subscriber } from "../../api/types";
import {
  badgeLow,
  hoverToDangerText,
  liveDotBg,
  offlineDotBg,
  secondaryButtonText,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { shortSessionId } from "../conversation/conversation-model";
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
  const confirmingTitle = confirming?.title.trim() ?? "";
  const confirmingLabel =
    confirming === undefined
      ? ""
      : confirmingTitle === ""
        ? shortSessionId(confirming.session_id)
        : confirmingTitle;

  if (subscribers.length === 0) {
    return null;
  }

  return (
    <section aria-label="Subscribed agents" className="mt-2">
      <h2 className={`text-sm font-semibold ${textSecondaryOnCanvas}`}>Subscribed agents</h2>
      <ul className="mt-2 flex flex-wrap gap-2">
        {subscribers.map((subscriber) => {
          const title = subscriber.title.trim();
          const label = title === "" ? shortSessionId(subscriber.session_id) : title;
          return (
            <li
              className={`flex items-center gap-2 rounded-full px-3 py-1 text-sm ${badgeLow.bg} ${badgeLow.text}`}
              key={subscriber.session_id}
            >
              <span
                aria-hidden="true"
                className={`h-2 w-2 shrink-0 rounded-full ${subscriber.live ? liveDotBg : offlineDotBg}`}
                title={subscriber.live ? "Live" : "Not live"}
              />
              <span title={subscriber.session_id}>{label}</span>
              <Timestamp at={new Date(subscriber.last_seen).toISOString()} />
              {subscriber.removable ? null : (
                <span className="text-xs italic">via {subscriber.via}</span>
              )}
              <button
                className={`font-medium ${secondaryButtonText} ${hoverToDangerText} disabled:cursor-not-allowed disabled:opacity-50`}
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
