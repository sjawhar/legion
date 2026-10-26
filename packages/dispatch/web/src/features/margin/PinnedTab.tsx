import type { ReactNode } from "react";

import type { Event } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import { borderDefault, textMutedOnSurface } from "../../theme/classes";
import { EventBody, isAskEvent } from "../activity/EventBody";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import type { PinnedConversationCard } from "./useMarginItems";

interface PinnedTabProps {
  /** Conversations pinned from the Conversation tab, each under its own `<kind>:<id>`. */
  conversations: PinnedConversationCard[];
  /** Events pinned from the Activity tab, and conversations pinned before a card carried its
   *  own id, which stay their event's line here until the reader pins the card again. */
  events: Event[];
  onUnpin(eventId: number): void;
  onUnpinConversation(id: string): void;
  pinnedIds: string[];
}

export function PinnedTab({
  conversations,
  events,
  onUnpin,
  onUnpinConversation,
  pinnedIds,
}: PinnedTabProps): ReactNode {
  return (
    <div className="pt-3">
      {conversations.map((conversation) => (
        <article className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={conversation.id}>
          <div className="flex items-start justify-between gap-2">
            <div className="min-w-0 flex-1">
              <MarkdownBody markdown={conversation.body} />
              <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
                <AgentName actor={conversation.author} inline /> ·{" "}
                <Timestamp at={conversation.at} />
              </p>
            </div>
            <PinButton
              label="Unpin"
              onClick={() => onUnpinConversation(conversation.id)}
              pinned
              quiet
            />
          </div>
        </article>
      ))}
      {events.map((event) => (
        <article className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={event.id}>
          <div className="flex items-start justify-between gap-2">
            <div className="min-w-0 flex-1">
              <EventBody event={event} />
              {isAskEvent(event) ? null : (
                <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
                  <AgentName actor={event.actor} inline />
                </p>
              )}
            </div>
            <PinButton label="Unpin" onClick={() => onUnpin(event.id)} pinned quiet />
          </div>
        </article>
      ))}
      {pinnedIds.length === 0 && conversations.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>No pinned items.</p>
      ) : null}
    </div>
  );
}
