import { replaceEqualDeep, useInfiniteQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { api } from "../../api/client";
import { type EventPages, mergeEventPages } from "../../api/event-feed";
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
import { usePinWrites } from "../issue/usePinWrites";
import { Timestamp } from "../refs/Timestamp";
import { ViewportAnchor } from "../shell/ViewportAnchor";
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
  const pins = usePinWrites(issueKey);
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
    // The feed (api/event-feed.ts) prepends events as they are published; a refetch whose read
    // predates one of them must not take it back.
    structuralSharing: (current, incoming) =>
      replaceEqualDeep(
        current,
        mergeEventPages(current as EventPages | undefined, incoming as EventPages)
      ),
  });
  const issueState = stateForIssue(state, issueKey);
  const events = useMemo(() => log.data?.pages.flat() ?? [], [log.data]);
  // Every row names its author, and a session's name is its live registry title, so the registry
  // is read whenever a row this tab is showing was written by one. A claim line names a second
  // session inside its own description, so it reads the registry too, whoever authored it.
  // Nothing is fetched for a history no session touched.
  const { titles } = useAgents(
    visible &&
      events.some(
        (event) =>
          event.actor.kind === "session" ||
          event.type === "issue.claimed" ||
          event.type === "issue.released"
      )
  );
  // The day the reader is in, recomputed every render so a midnight rollover moves the `Today`
  // label; the model itself is rebuilt only when one of its inputs changes, since building it
  // copies, sorts, describes and re-walks the whole history.
  const today = dateKey(new Date().toISOString());
  const items = useMemo(
    () =>
      buildActivityItems({
        events,
        lastReadSeq: issueState.last_read_seq,
        titles,
        today,
      }),
    [events, issueState.last_read_seq, titles, today]
  );

  const togglePin = (eventId: number) =>
    pins.updatePins((dismissed) => ({
      id: eventItemId({ id: eventId }),
      op: isPinnedEvent(dismissed, eventId) ? "unpin" : "pin",
    }));

  if (log.isPending) {
    return <p className={textMutedOnCanvas}>Loading activity…</p>;
  }
  if (log.isError) {
    return <p className={dangerText}>Could not load the activity.</p>;
  }

  // The stream prepends each published event to the front of page 0 (`prependEventToLog`) and
  // this list is newest first, so an arrival pushes every row below it down. The anchor holds
  // the row the reader is on - each row carries its own `data-event-seq` - where it was.
  return (
    <ViewportAnchor
      as="section"
      className="flex min-h-[60dvh] flex-col gap-3 pb-16"
      item="data-event-seq"
      label="Activity"
    >
      {pins.banner}
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

          const author = resolveAuthor(item.author, titles).label;
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
                <span className="min-w-0 truncate font-medium" title={author}>
                  {author}
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
                disabled={pins.blocked}
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
    </ViewportAnchor>
  );
}
