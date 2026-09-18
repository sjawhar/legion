import type { ReactNode } from "react";

import type { Event } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import { borderDefault, textMutedOnSurface } from "../../theme/classes";
import { EventBody, isAskEvent } from "../issue/EventBody";
import { actorLabel } from "../refs/actor";

interface PinnedTabProps {
  events: Event[];
  onUnpin(eventId: number): void;
  pinnedIds: string[];
}

export function PinnedTab({ events, onUnpin, pinnedIds }: PinnedTabProps): ReactNode {
  return (
    <div className="pt-3">
      {events.map((event) => (
        <article className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={event.id}>
          <div className="flex items-start justify-between gap-2">
            <div className="min-w-0 flex-1">
              <EventBody event={event} />
              {isAskEvent(event) ? null : (
                <p className={`mt-1 text-xs ${textMutedOnSurface}`}>{actorLabel(event.actor)}</p>
              )}
            </div>
            <PinButton label="Unpin" onClick={() => onUnpin(event.id)} pinned quiet />
          </div>
        </article>
      ))}
      {pinnedIds.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>No pinned items.</p>
      ) : null}
    </div>
  );
}
