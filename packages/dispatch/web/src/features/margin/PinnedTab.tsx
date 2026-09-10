import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { Event, UserState } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { EventBody, isAskEvent } from "../issue/EventBody";
import { dismissedEventIds, eventDescription } from "../issue/log-model";
import { actorLabel } from "../refs/actor";

interface PinnedTabProps {
  events: Event[];
  issueKey: string | undefined;
  pinnedIds: string[];
}

export function PinnedTab({ events, issueKey, pinnedIds }: PinnedTabProps): ReactNode {
  const queryClient = useQueryClient();
  const [showDismissed, setShowDismissed] = useState(false);
  const userState = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["user-state"],
    queryFn: () => api.getMyState(),
  });
  const dismissedIds = dismissedEventIds(userState.data?.[issueKey ?? ""]?.dismissed ?? []);
  const dismissedEvents = useQuery({
    enabled: issueKey !== undefined && showDismissed && dismissedIds.length > 0,
    queryKey: ["events", issueKey, "margin-dismissed", dismissedIds],
    queryFn: () => api.getIssueEvents(issueKey ?? "", { ids: dismissedIds }),
  });
  const undismiss = useMutation({
    mutationFn: (id: number) => {
      const marker = `event:${id}`;
      const current = userState.data?.[issueKey ?? ""]?.dismissed ?? [];
      return api.putIssueState(issueKey ?? "", {
        dismissed: current.filter((item) => item !== marker),
      });
    },
    onSuccess: (next) => {
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [issueKey ?? ""]: next,
      }));
    },
  });

  return (
    <div className="pt-3">
      {events.map((event) => (
        <article className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={event.id}>
          <EventBody event={event} />
          {isAskEvent(event) ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>{actorLabel(event.actor)}</p>
          )}
        </article>
      ))}
      {pinnedIds.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>No pinned items.</p>
      ) : null}
      <label className={`mt-3 flex items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
        <input
          checked={showDismissed}
          onChange={(event) => setShowDismissed(event.target.checked)}
          type="checkbox"
        />
        Show dismissed
      </label>
      {showDismissed ? (
        <div className="mt-2 space-y-2">
          {(dismissedEvents.data ?? []).map((event) => (
            <article
              className={`flex items-center justify-between gap-3 rounded-lg border p-3 text-sm ${borderDefault}`}
              key={event.id}
            >
              <span>{eventDescription(event)}</span>
              <button
                className={`shrink-0 ${linkText} ${linkHoverText}`}
                disabled={undismiss.isPending}
                onClick={() => undismiss.mutate(event.id)}
                type="button"
              >
                Restore
              </button>
            </article>
          ))}
          {dismissedIds.length === 0 ? (
            <p className={`text-sm ${textMutedOnSurface}`}>No dismissed items.</p>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
