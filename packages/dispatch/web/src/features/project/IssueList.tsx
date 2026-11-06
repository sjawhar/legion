import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { projectIssuesQuery, userStateQuery } from "../../api/queries";
import type { IssueSummary } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill, pillClassName } from "../../components/Pill";
import {
  borderDefault,
  cardHoverBorder,
  checkboxAccent,
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { setSelectedIssues } from "../search/bar-scope";
import { useKeymap } from "../shell/keymap";
import { closestMatching, reachableRows, roveFocus } from "../shell/roving";
import { issueStatuses, statusLabel } from "./board-model";
import { useIssueFilters } from "./issue-filters";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

/** What each row is marked with, for the keys that rove them. */
const ROW_SELECTOR = "[data-issue-row]";

/** A row is a keyboard target for the `j`/`k`/`o`/`Enter`/`x` this file registers: `tabIndex={-1}`
 *  so those keys reach it and Tab does not, and `data-issue-row` names it for them, as the board's
 *  cards do. The focus ring is the row's own, since the row is what focus lands on. */
function IssueRow({
  issue,
  marked,
  onMark,
  unread,
}: {
  issue: IssueSummary;
  /** Whether the reader has selected this issue, which is what the bar says it will act on. */
  marked: boolean;
  /** Selects or deselects it; `x` and this checkbox are the two ways in, as the Inbox's are. */
  onMark: (key: string) => void;
  unread: boolean;
}): ReactNode {
  return (
    <li
      aria-label={`${issue.key} ${issue.title}`}
      className={`flex min-w-0 items-start gap-2 border-t py-3 outline-none first:border-t-0 focus-visible:ring-2 ${borderDefault} ${focusVisibleRing}`}
      data-issue-row={issue.key}
      tabIndex={-1}
    >
      <input
        aria-label={`Select ${issue.key}`}
        checked={marked}
        className={`mt-1 size-4 shrink-0 ${checkboxAccent}`}
        onChange={() => onMark(issue.key)}
        type="checkbox"
      />
      {/* One grid, two arrangements. From `sm` the reference and the timestamp share the first
          line and the metadata runs below. On a phone the reference takes the whole first line
          and the timestamp moves down beside the metadata: beside the title it would leave the
          title a column about 130 px wide. */}
      <div className="grid min-w-0 flex-1 grid-cols-[minmax(0,1fr)_auto] items-start gap-2">
        <Link
          className={`col-span-2 row-start-1 min-w-0 text-sm sm:col-span-1 ${linkText} ${linkHoverText}`}
          data-issue-link
          to={buildIssuePath({ key: issue.key, kind: "issue" })}
          {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
        >
          {/* Below 1280 px a link is an inline-flex box (`styles.css`), so the key and the title
              would be two flex columns. One wrapper keeps them one run of text that wraps across
              the row, and the key never breaks at its hyphen. */}
          <span className="min-w-0 break-words">
            <span className="font-semibold whitespace-nowrap">{issue.key}</span>
            <span className={`ml-2 ${textPrimaryOnCanvas}`}>{issue.title}</span>
          </span>
        </Link>
        <div
          className={`col-start-2 row-start-2 flex items-center gap-2 self-center text-xs sm:row-start-1 sm:self-start ${textMutedOnCanvas}`}
        >
          {unread ? <UnreadDot /> : null}
          {issue.open_asks === 0 ? null : <AttentionBadge count={issue.open_asks} />}
          <Timestamp at={issue.updated_at} />
        </div>
        <div className="col-start-1 row-start-2 flex min-w-0 flex-wrap items-center gap-2 sm:col-span-2">
          <PriorityControl
            disabled={issue.status === "done"}
            issueKey={issue.key}
            priority={issue.priority}
          />
          <ClaimChip claim={issue.claim} />
          <ProgressChips progress={issue.progress} />
          <UnreachableRouteMarker issue={issue} />
          {(issue.labels ?? []).map((label) => (
            <LabelPill key={label}>{label}</LabelPill>
          ))}
          {issue.parent === null ? null : (
            <Link
              className={`${pillClassName("label")} border ${borderDefault} ${cardHoverBorder}`}
              to={buildIssuePath({ key: issue.parent, kind: "issue" })}
              {...referenceTriggerProps({ key: issue.parent, kind: "issue" })}
            >
              {issue.parent}
            </Link>
          )}
        </div>
      </div>
    </li>
  );
}

export function IssueList({ project }: { project: string }): ReactNode {
  const { labels, matches, statuses } = useIssueFilters();
  const allIssues = useQuery(projectIssuesQuery(project, []));
  const filteredIssues = useQuery({
    ...projectIssuesQuery(project, labels),
    enabled: labels.length > 0,
  });
  const issues = labels.length === 0 ? allIssues : filteredIssues;
  const state = useQuery(userStateQuery());
  const visibleIssues = useMemo(
    () =>
      (issues.data ?? []).filter(
        (issue) =>
          (statuses.length === 0 || statuses.includes(issue.status)) &&
          matches(issue, state.data?.[issue.key]?.last_read_seq ?? 0)
      ),
    [issues.data, matches, state.data, statuses]
  );
  // The issues the reader has selected, by key: `x` and a row's own checkbox are the two ways
  // in, as the Inbox's marks are. A key the list no longer holds - another filter, another
  // project - drops out, so a row that is listed again comes back unselected.
  const [marked, setMarked] = useState<ReadonlySet<string>>(() => new Set());
  const onMark = useCallback((key: string) => {
    setMarked((current) => {
      const next = new Set(current);
      if (!next.delete(key)) next.add(key);
      return next;
    });
  }, []);
  const listedKeys = visibleIssues.map((issue) => issue.key).join(" ");
  useEffect(() => {
    const listed = new Set(listedKeys === "" ? [] : listedKeys.split(" "));
    setMarked((current) =>
      [...current].every((key) => listed.has(key))
        ? current
        : new Set([...current].filter((key) => listed.has(key)))
    );
  }, [listedKeys]);
  // What the bar says it will act on when it opens over this list. It belongs to the list while
  // the list is on the page and to nobody after that, so leaving the page clears it.
  const selectedKeys = [...marked].join(" ");
  useEffect(() => {
    setSelectedIssues(selectedKeys === "" ? [] : selectedKeys.split(" "));
    return () => setSelectedIssues([]);
  }, [selectedKeys]);

  // The keys that rove these rows register with the page's `project` scope, and they live
  // here because this component mounts only while the List is the view showing: on the Board,
  // whose own `j`/`k`/`o`/`Enter` act on its cards, `?` would otherwise list each key twice.
  // `roveFocus` and these `when`s read one rule for which rows count, so a key is offered
  // exactly while it moves: the rows of a collapsed status band are in the DOM, take no focus,
  // and are no step.
  const rows = () => [...document.querySelectorAll<HTMLElement>(ROW_SELECTOR)];
  const focusedRow = () => closestMatching(document.activeElement, ROW_SELECTOR);
  // The title link, by its mark: the row holds other links (a claim's agent, the parent), and
  // `o`/`Enter` must open the issue whichever comes first.
  const openFocusedRow = () =>
    focusedRow()?.querySelector<HTMLElement>("[data-issue-link]")?.click();
  useKeymap("project", [
    // Movement: a palette row that moves the cursor helps nobody.
    {
      id: "list-next",
      keys: "j",
      label: "Next issue",
      palette: false,
      run: () => roveFocus(rows(), focusedRow(), 1),
      when: () => reachableRows(rows()).length > 0,
    },
    {
      id: "list-previous",
      keys: "k",
      label: "Previous issue",
      palette: false,
      run: () => roveFocus(rows(), focusedRow(), -1),
      when: () => reachableRows(rows()).length > 0,
    },
    {
      id: "list-open",
      keys: "o",
      label: "Open issue",
      run: openFocusedRow,
      when: () => focusedRow() !== null,
    },
    {
      // Only from the row itself: Enter on its title link is the browser's own navigation. Out of
      // the palette, where `list-open` above is already the row for the same action.
      id: "list-open-enter",
      keys: "Enter",
      label: "Open the focused issue",
      palette: false,
      run: openFocusedRow,
      when: () => document.activeElement?.matches(ROW_SELECTOR) === true,
    },
    {
      // Selecting walks the list with the row in hand, as `j`/`k` do, so it is no palette row.
      id: "list-select",
      keys: "x",
      label: "Select or deselect the focused issue",
      palette: false,
      run: () => {
        const key = focusedRow()?.dataset.issueRow;
        if (key !== undefined) onMark(key);
      },
      when: () => focusedRow() !== null,
    },
  ]);

  if (issues.isPending || state.isPending) {
    return <LoadingSkeleton label="Loading issues" />;
  }
  if (issues.isError || state.isError) {
    return <p className={dangerText}>Could not load issues.</p>;
  }

  return (
    <section aria-label="Project issues">
      {issueStatuses.map((currentStatus) => {
        const grouped = visibleIssues.filter((issue) => issue.status === currentStatus);
        if (grouped.length === 0) {
          return null;
        }
        return (
          // A band is a divider, not a card: a bordered box with 16 px of padding around one
          // or two rows spent more height on the grouping than on the issues. The label is the
          // list's section-label role, quieter than the rows it introduces.
          <details
            aria-label={`${statusLabel(currentStatus)} (${grouped.length})`}
            className="mb-4"
            key={currentStatus}
            open={currentStatus !== "done"}
          >
            {/* No `flex`: it would set `display: flex` on the summary and take the native
                disclosure marker with it, leaving a collapsed band looking like a plain
                heading with nothing to say it opens. */}
            <summary
              className={`min-h-11 cursor-pointer py-3.5 text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}
            >
              {statusLabel(currentStatus)} ({grouped.length})
            </summary>
            <ul
              aria-label={`${statusLabel(currentStatus)} issues`}
              className={`rounded-xl border px-4 ${borderDefault}`}
            >
              {grouped.map((issue) => (
                <IssueRow
                  issue={issue}
                  key={issue.key}
                  marked={marked.has(issue.key)}
                  onMark={onMark}
                  unread={issueIsUnread(issue, state.data?.[issue.key]?.last_read_seq ?? 0)}
                />
              ))}
            </ul>
          </details>
        );
      })}
      {visibleIssues.length === 0 ? (
        <EmptyState label="No matching project issues" message="No issues match these filters." />
      ) : null}
    </section>
  );
}
