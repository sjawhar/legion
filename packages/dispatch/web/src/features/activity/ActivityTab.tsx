import { replaceEqualDeep, useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import { type EventPages, mergeEventPages } from "../../api/sse";
import type { UserState } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import {
  dangerText,
  linkText,
  newDividerLine,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { resolveAuthor } from "../conversation/authors";
import { useAgents } from "../conversation/useAgents";
import { eventItemId, isPinnedEvent, stateForIssue } from "../issue/pins";
import {
  applyPinStateOperation,
  issueStateTransport,
  type PinStateOperation,
  sharedIssueStateWrites,
} from "../issue/state-write-queue";
import { Timestamp } from "../refs/Timestamp";
import { ActivityLine } from "./ActivityLine";
import { buildActivityItems, dateKey } from "./activity-model";

interface ActivityTabProps {
  issueKey: string;
  state: UserState | undefined;
  /** Whether the tab's panel is the one on screen; the agent registry is read only while it is. */
  visible: boolean;
}

/**
 * The issue's whole event history: every event one line, newest first, broken by day and by the
 * reader's unread watermark. It writes no read mark - the Conversation is `last_read_seq`'s only
 * writer, so the watermark this draws has exactly one author - and it offers no composer and no
 * filters: this is the record, not a place to write.
 */
export function ActivityTab({ issueKey, state, visible }: ActivityTabProps): ReactNode {
  const queryClient = useQueryClient();
  const log = useInfiniteQuery({
    initialPageParam: null as number | null,
    queryKey: ["events", issueKey],
    queryFn: ({ pageParam }) =>
      api.getIssueEvents(
        issueKey,
        pageParam === null ? { limit: 200, order: "desc" } : { before: pageParam, limit: 200 }
      ),
    // A page can hold more than the server's 200 once streamed events sit above a full response
    // (mergeEventPages, prependEventToLog); it is still a full page with older events beneath it.
    getNextPageParam: (page) => (page.length >= 200 ? page.at(-1)?.seq : undefined),
    // The stream (api/sse.ts) prepends events as they are published; a refetch whose read
    // predates one of them must not take it back.
    structuralSharing: (current, incoming) =>
      replaceEqualDeep(
        current,
        mergeEventPages(current as EventPages | undefined, incoming as EventPages)
      ),
  });
  const issueState = stateForIssue(state, issueKey);
  const events = log.data?.pages.flat() ?? [];
  // A claim line names a session, so it reads the registry like every other author line; nothing
  // is fetched for a history with no claim in it.
  const { titles } = useAgents(
    visible &&
      events.some((event) => event.type === "issue.claimed" || event.type === "issue.released")
  );
  const items = buildActivityItems({
    events,
    lastReadSeq: issueState.last_read_seq,
    titles,
    today: dateKey(new Date().toISOString()),
  });

  const togglePin = (eventId: number) => {
    let operation: PinStateOperation | undefined;
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      const currentIssueState = stateForIssue(current, issueKey);
      operation = {
        id: eventItemId({ id: eventId }),
        op: isPinnedEvent(currentIssueState.dismissed, eventId) ? "unpin" : "pin",
      };
      return {
        ...current,
        [issueKey]: {
          ...currentIssueState,
          dismissed: applyPinStateOperation(currentIssueState.dismissed, operation),
        },
      };
    });
    if (operation === undefined) {
      return;
    }
    void sharedIssueStateWrites
      .enqueue(issueKey, operation, {
        ...issueStateTransport,
        onDrained: (key, next) => {
          queryClient.setQueryData<UserState>(["user-state"], (current) => ({
            ...current,
            [key]: next,
          }));
        },
        onError: () => {
          void queryClient.invalidateQueries({ queryKey: ["user-state"] });
        },
      })
      .catch(() => {});
  };

  if (log.isPending) {
    return <p className={textMutedOnCanvas}>Loading activity…</p>;
  }
  if (log.isError) {
    return <p className={dangerText}>Could not load the activity.</p>;
  }

  return (
    <section className="flex min-h-[60dvh] flex-col gap-3 pb-16">
      <ol aria-label="Issue activity" className="flex flex-col gap-1">
        {items.map((item) => {
          if (item.kind === "day-divider" || item.kind === "unread-divider") {
            const label = item.kind === "day-divider" ? item.label : "New since you last read";
            return (
              // biome-ignore lint/a11y/useSemanticElements: A labeled list item is required by the activity DOM contract.
              // biome-ignore lint/a11y/useFocusableInteractive: A divider is descriptive, not an interactive control.
              <li
                aria-label={label}
                className={`my-2 flex items-center gap-3 text-xs font-semibold tracking-wide uppercase ${
                  item.kind === "unread-divider" ? linkText : textMutedOnCanvas
                }`}
                key={item.id}
                // biome-ignore lint/a11y: The role is required by the activity DOM contract.
                role="separator"
              >
                <span className={`h-px flex-1 ${newDividerLine}`} />
                {label}
                <span className={`h-px flex-1 ${newDividerLine}`} />
              </li>
            );
          }

          const pinned = isPinnedEvent(issueState.dismissed, item.event.id);
          return (
            <li
              className={`flex flex-wrap items-baseline gap-x-2 px-2 text-xs ${textMutedOnCanvas}`}
              data-event-seq={item.seq}
              key={item.id}
            >
              {/* Who and when are one group that never breaks, so the time never wraps alone to
                  the start of the next row when the author takes most of the line. The author
                  truncates inside it, with the full label on hover - a live session title can
                  be a whole sentence - and only the description wraps, with a floor so it
                  wraps as prose rather than one word per line. */}
              <span className="flex max-w-full min-w-0 shrink-0 items-baseline gap-x-2 whitespace-nowrap">
                <span
                  className="min-w-0 truncate font-medium"
                  title={resolveAuthor(item.author, titles).label}
                >
                  {resolveAuthor(item.author, titles).label}
                </span>
                <span aria-hidden="true">·</span>
                <Timestamp at={item.at} />
              </span>
              <span className="min-w-48 flex-1 break-words">
                <ActivityLine
                  description={item.description}
                  event={item.event}
                  issueKey={issueKey}
                />
              </span>
              <PinButton
                label={pinned ? "Unpin" : "Pin"}
                onClick={() => togglePin(item.event.id)}
                pinned={pinned}
                quiet
              />
            </li>
          );
        })}
      </ol>
      {items.length === 0 ? <p className={textMutedOnCanvas}>No activity yet.</p> : null}
      {log.hasNextPage ? (
        <div className="flex items-center justify-center">
          <button
            className={`min-h-11 rounded-lg px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            disabled={log.isFetchingNextPage}
            onClick={() => void log.fetchNextPage()}
            type="button"
          >
            {log.isFetchingNextPage ? "Loading…" : "Load older"}
          </button>
        </div>
      ) : null}
    </section>
  );
}
