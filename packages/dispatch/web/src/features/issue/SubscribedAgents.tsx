import { type ReactNode, useState } from "react";

import type { Subscriber } from "../../api/types";
import { textSecondaryOnSurface } from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { sessionChipClassName, sessionRemoveButtonClassName } from "../refs/SessionChip";
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
 * family, each drawn by `AgentName` (its name, its running state from the agent registry, and
 * a link to its live conversation), how long ago it was last seen, and an Unsubscribe control
 * that confirms before telling the session it was removed. The subscribers endpoint says who
 * is subscribed, whether each can be removed here, and the title each stored; it does not say
 * whether one is running, so one agent never shows two states on one page.
 */
export function SubscribedAgents({
  onUnsubscribe,
  ownerLabel,
  subscribers,
}: SubscribedAgentsProps): ReactNode {
  const [confirming, setConfirming] = useState<Subscriber | undefined>(undefined);
  const text = useAgentText();
  const confirmingLabel =
    confirming === undefined ? "" : text.sessionText(confirming.session_id, confirming.title);

  if (subscribers.length === 0) {
    return null;
  }

  return (
    <section aria-label="Subscribed agents" className="mt-2">
      <h2 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>Subscribed agents</h2>
      <ul className="mt-2 flex flex-wrap gap-2">
        {subscribers.map((subscriber) => (
          <li className={sessionChipClassName} key={subscriber.session_id}>
            <span title={subscriber.session_id}>
              <AgentName sessionId={subscriber.session_id} storedTitle={subscriber.title} />
            </span>
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
        ))}
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
