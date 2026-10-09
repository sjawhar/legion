import { type ReactNode, useLayoutEffect, useMemo, useRef, useState } from "react";

import type { DeliveryPR } from "../../api/types";
import {
  borderDefault,
  dangerText,
  inlineWarningText,
  linkText,
  successText,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { type ColorFacet, colorKeyFor, PLACEHOLDER_COLOR } from "./lib/colorScale";
import { buildPRListRows, compareIssueSort, formatMinutes, type PRListRow } from "./lib/prList";

/** Every body row is this tall, so the window of rows on screen is arithmetic, with no measuring. */
const ROW_HEIGHT_PX = 36;
/** Rows rendered past each edge of the scrolled-to window. */
const OVERSCAN_ROWS = 12;

type ColumnId =
  | "color"
  | "merged"
  | "pr"
  | "title"
  | "issue"
  | "parentAgent"
  | "author"
  | "size"
  | "deployed"
  | "leadTime"
  | "rework";

type SortId = Exclude<ColumnId, "color">;

// Column widths, shared by header and body cells so the flex rows line up; Title takes the rest.
// Sized (with the px-1.5 cell padding) to fit their longest usual content untruncated at text-xs:
// "deployed Sep 26, 10:48 PM", "ACME-1028 · No priority", a parent agent's title, and a sortable
// header's label plus its arrow.
const COLUMNS: { id: ColumnId; label: string; className: string }[] = [
  { id: "color", label: "", className: "w-5 shrink-0" },
  { id: "merged", label: "Merged", className: "w-28 shrink-0" },
  { id: "pr", label: "PR", className: "w-24 shrink-0" },
  { id: "title", label: "Title", className: "min-w-32 flex-1" },
  { id: "issue", label: "Issue", className: "w-40 shrink-0" },
  { id: "parentAgent", label: "Parent agent", className: "w-28 shrink-0" },
  { id: "author", label: "Author", className: "w-29 shrink-0" },
  { id: "size", label: "Size", className: "w-22 shrink-0" },
  { id: "deployed", label: "Deployed", className: "w-41 shrink-0" },
  { id: "leadTime", label: "Lead time", className: "w-21 shrink-0" },
  { id: "rework", label: "Rework", className: "w-18 shrink-0" },
];
const COLUMN_CLASS = Object.fromEntries(COLUMNS.map((column) => [column.id, column.className]));

const DEPLOYED_TONE: Record<DeliveryPR["deployed_status"], string> = {
  deployed: successText,
  waiting: inlineWarningText,
  not_tracked: textMutedOnSurface,
};

const dateTime = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

type SortValue = number | string | boolean | undefined;

function sortValue(row: PRListRow, id: SortId): SortValue {
  switch (id) {
    case "merged":
      return row.mergedMs;
    case "pr":
      return row.prLabel;
    case "title":
      return row.pr.title;
    case "issue":
      return undefined; // compared by compareIssueSort below
    case "parentAgent":
      return row.parentAgentSort;
    case "author":
      return row.pr.author;
    case "size":
      return row.size;
    case "deployed":
      return row.deployedMs;
    case "leadTime":
      return row.leadTimeMinutes;
    case "rework":
      return row.pr.rework;
  }
}

/** A row with no value for the column (no issue, not deployed, a placeholder agent) sorts last in
 *  either direction; text compares with numeric runs as numbers. */
function compareRows(a: PRListRow, b: PRListRow, id: SortId, desc: boolean): number {
  const direction = desc ? -1 : 1;
  if (id === "issue") {
    if (a.issueSort === undefined || b.issueSort === undefined) {
      return Number(a.issueSort === undefined) - Number(b.issueSort === undefined);
    }
    return direction * compareIssueSort(a.issueSort, b.issueSort);
  }
  const left = sortValue(a, id);
  const right = sortValue(b, id);
  if (left === undefined || right === undefined) {
    return Number(left === undefined) - Number(right === undefined);
  }
  if (typeof left === "string" && typeof right === "string") {
    return direction * left.localeCompare(right, undefined, { numeric: true });
  }
  return direction * (Number(left) - Number(right));
}

function Cell({ row, id, color }: { row: PRListRow; id: ColumnId; color: string }): ReactNode {
  const { pr } = row;
  switch (id) {
    case "color":
      return (
        <span
          aria-hidden="true"
          className="block size-2 rounded-full"
          style={{ backgroundColor: color }}
        />
      );
    case "merged":
      return row.mergedMs === undefined ? null : (
        <span title={new Date(row.mergedMs).toLocaleString()}>{dateTime.format(row.mergedMs)}</span>
      );
    case "pr":
      return (
        <a
          className={`truncate hover:underline ${linkText}`}
          href={pr.url}
          onClick={(event) => event.stopPropagation()}
          rel="noreferrer"
          target="_blank"
          title={pr.id}
        >
          {row.prLabel}
        </a>
      );
    case "title":
      return (
        <span className="truncate" title={pr.title}>
          {pr.title}
        </span>
      );
    case "issue":
      return (
        <span className={`truncate ${row.issueSort === undefined ? textMutedOnSurface : ""}`}>
          {row.issueLabel}
        </span>
      );
    case "parentAgent":
      return (
        <span
          className={`truncate ${row.parentAgentSort === undefined ? textMutedOnSurface : ""}`}
          title={row.parentAgentLabel}
        >
          {row.parentAgentLabel}
        </span>
      );
    case "author":
      return <span className="truncate">{pr.author}</span>;
    case "size":
      return pr.additions === null || pr.deletions === null ? null : (
        <span className="tabular-nums">
          <span className={successText}>+{pr.additions}</span>{" "}
          <span className={dangerText}>-{pr.deletions}</span>
        </span>
      );
    case "deployed":
      return (
        <span className="truncate">
          <span className={DEPLOYED_TONE[pr.deployed_status]}>{row.deployedLabel}</span>
          {row.deployedMs === undefined ? null : (
            <span className={textSecondaryOnSurface}> {dateTime.format(row.deployedMs)}</span>
          )}
        </span>
      );
    case "leadTime":
      return row.leadTimeMinutes === undefined ? null : (
        <span className="tabular-nums" title="Merge to production">
          {formatMinutes(row.leadTimeMinutes)}
        </span>
      );
    case "rework":
      return pr.rework ? <span className={inlineWarningText}>rework</span> : null;
  }
}

/** The filtered PRs as a sortable table, one line per row, coloured by the same facet as the
 *  timeline; only the rows scrolled into view are rendered. A row opens the DrillDown. */
export function PRList({
  prs,
  allRepos,
  colorBy,
  colorScale,
  selectedId,
  onSelect,
}: {
  prs: readonly DeliveryPR[];
  allRepos: readonly string[];
  colorBy: ColorFacet;
  colorScale: Map<string, string>;
  selectedId: string | undefined;
  onSelect: (id: string) => void;
}): ReactNode {
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const [sort, setSort] = useState<{ id: SortId; desc: boolean }>({ id: "merged", desc: true });
  const [scrollTop, setScrollTop] = useState(0);
  // Unmeasured (the first render, or a DOM with no layout), the window is the browser's height.
  const [viewportHeight, setViewportHeight] = useState(() => window.innerHeight);

  const rows = useMemo(() => buildPRListRows(prs, allRepos), [prs, allRepos]);
  const sorted = useMemo(
    () => [...rows].sort((a, b) => compareRows(a, b, sort.id, sort.desc)),
    [rows, sort]
  );

  useLayoutEffect(() => {
    const element = scrollRef.current;
    if (element === null) return;
    const measure = () => {
      if (element.clientHeight > 0) setViewportHeight(element.clientHeight);
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  const first = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT_PX) - OVERSCAN_ROWS);
  const last = Math.min(
    sorted.length,
    Math.ceil((scrollTop + viewportHeight) / ROW_HEIGHT_PX) + OVERSCAN_ROWS
  );

  const toggleSort = (id: SortId) =>
    setSort((current) => (current.id === id ? { id, desc: !current.desc } : { id, desc: false }));

  return (
    <div
      className={`min-h-0 flex-1 overflow-auto rounded border max-xl:h-[70dvh] max-xl:flex-none ${borderDefault} ${surfaceBg} ${textPrimaryOnSurface}`}
      onScroll={(event) => setScrollTop(event.currentTarget.scrollTop)}
      ref={scrollRef}
    >
      <table aria-rowcount={sorted.length + 1} className="grid min-w-[1152px] text-xs">
        <thead className={`sticky top-0 z-10 grid ${surfaceBg}`}>
          <tr aria-rowindex={1} className={`flex w-full border-b ${borderDefault}`}>
            {COLUMNS.map((column) => {
              if (column.id === "color") {
                return (
                  <th
                    className={`flex h-9 items-center px-1.5 ${column.className}`}
                    key={column.id}
                  />
                );
              }
              const id = column.id;
              const sorted = sort.id === id;
              let ariaSort: "ascending" | "descending" | undefined;
              if (sorted) ariaSort = sort.desc ? "descending" : "ascending";
              let arrow = "\u2195";
              if (sorted) arrow = sort.desc ? "\u2193" : "\u2191";
              return (
                <th
                  aria-sort={ariaSort}
                  className={`flex h-9 items-center px-1.5 font-medium ${column.className}`}
                  key={id}
                >
                  <button
                    className={`flex items-center gap-1 ${sorted ? textPrimaryOnSurface : textMutedOnSurface}`}
                    onClick={() => toggleSort(id)}
                    type="button"
                  >
                    {column.label}
                    <span aria-hidden="true" className={sorted ? "" : "opacity-40"}>
                      {arrow}
                    </span>
                  </button>
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody className="relative grid" style={{ height: sorted.length * ROW_HEIGHT_PX }}>
          {sorted.length === 0 ? (
            <tr className="absolute flex h-9 w-full items-center">
              <td className={`px-2 ${textMutedOnSurface}`}>No PRs in the current filter/window.</td>
            </tr>
          ) : null}
          {sorted.slice(first, last).map((row, offset) => {
            const index = first + offset;
            const select = () => onSelect(row.pr.id);
            const color = colorScale.get(colorKeyFor(row.pr, colorBy)) ?? PLACEHOLDER_COLOR;
            return (
              <tr
                aria-rowindex={index + 2}
                aria-selected={row.pr.id === selectedId}
                className={`absolute flex h-9 w-full cursor-pointer items-center border-b outline-none ${borderDefault} ${row.pr.id === selectedId ? surfaceMutedBg : surfaceMutedHoverBg}`}
                key={row.pr.id}
                onClick={select}
                onKeyDown={(event) => {
                  // Only the row itself: Enter on the PR link inside it opens GitHub.
                  if (event.key === "Enter" && event.target === event.currentTarget) select();
                }}
                style={{ transform: `translateY(${index * ROW_HEIGHT_PX}px)` }}
                tabIndex={0}
              >
                {COLUMNS.map((column) => (
                  <td
                    className={`flex min-w-0 items-center px-1.5 py-0 whitespace-nowrap ${COLUMN_CLASS[column.id]}`}
                    key={column.id}
                  >
                    <Cell color={color} id={column.id} row={row} />
                  </td>
                ))}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
