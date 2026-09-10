import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { Event, UserState } from "../../api/types";
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
    <div className="pt-3 md:max-h-[45dvh] md:overflow-y-auto">
      {events.map((event) => (
        <article className="rounded-lg border border-slate-200 p-3 text-sm" key={event.id}>
          <p className="font-medium text-slate-900">{eventDescription(event)}</p>
          <p className="mt-1 text-xs text-slate-500">{actorLabel(event.actor)}</p>
        </article>
      ))}
      {pinnedIds.length === 0 ? <p className="text-sm text-slate-500">No pinned items.</p> : null}
      <label className="mt-3 flex items-center gap-2 text-sm text-slate-600">
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
              className="flex items-center justify-between gap-3 rounded-lg border border-slate-200 p-3 text-sm"
              key={event.id}
            >
              <span>{eventDescription(event)}</span>
              <button
                className="shrink-0 text-sky-700 hover:text-sky-900"
                disabled={undismiss.isPending}
                onClick={() => undismiss.mutate(event.id)}
                type="button"
              >
                Restore
              </button>
            </article>
          ))}
          {dismissedIds.length === 0 ? (
            <p className="text-sm text-slate-500">No dismissed items.</p>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
