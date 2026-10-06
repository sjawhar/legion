import { type ReactNode, useMemo, useState } from "react";
import type { DeliveryPR } from "../../api/types";
import {
  borderDefault,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildColorScale, type ColorFacet, colorKeyFor } from "./lib/colorScale";
import { buildPRListRows, type PRListRow } from "./lib/prList";

type SortKey =
  | "merged"
  | "pr"
  | "issue"
  | "parentAgent"
  | "author"
  | "size"
  | "deployed"
  | "leadTime"
  | "rework";

interface Column {
  key: SortKey;
  label: string;
  className: string;
}

const COLUMNS: Column[] = [
  { key: "merged", label: "Merged", className: "w-36 shrink-0" },
  { key: "pr", label: "PR", className: "w-28 shrink-0" },
  { key: "issue", label: "Issue", className: "w-40 shrink-0" },
  { key: "parentAgent", label: "Parent agent", className: "w-40 shrink-0" },
  { key: "author", label: "Author", className: "w-32 shrink-0" },
  { key: "size", label: "Size", className: "w-20 shrink-0 text-right" },
  { key: "deployed", label: "Deployed", className: "w-40 shrink-0" },
  { key: "leadTime", label: "Lead time", className: "w-28 shrink-0 text-right" },
  { key: "rework", label: "Rework", className: "w-20 shrink-0" },
];

const dateTime = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});

function formatMinutes(minutes: number | undefined): string {
  if (minutes === undefined) return "\u2014";
  if (minutes < 60) return `${Math.round(minutes)} min`;
  return `${(minutes / 60).toFixed(1)} h`;
}

function sortValue(row: PRListRow, key: SortKey): number | string | undefined {
  switch (key) {
    case "merged":
      return row.mergedMs;
    case "pr":
      return row.prLabel;
    case "issue":
      return row.issueSort ?? row.issueLabel;
    case "parentAgent":
      return row.parentAgentSort ?? row.parentAgentLabel;
    case "author":
      return row.pr.author;
    case "size":
      return row.size;
    case "deployed":
      return row.deployedMs;
    case "leadTime":
      return row.leadTimeMinutes;
    case "rework":
      return row.pr.rework ? 1 : 0;
    default:
      return undefined;
  }
}

function compareRows(a: PRListRow, b: PRListRow, key: SortKey, direction: 1 | -1): number {
  const left = sortValue(a, key);
  const right = sortValue(b, key);
  if (left === undefined && right === undefined) return 0;
  if (left === undefined) return 1; // undefined (no value yet) always sorts last
  if (right === undefined) return -1;
  if (typeof left === "string" && typeof right === "string") {
    return direction * left.localeCompare(right, undefined, { numeric: true });
  }
  return direction * ((left as number) - (right as number));
}

/** The filtered PRs as a sortable table; a row click opens the DrillDown. Plain client-side
 *  sort — this slice adds no table/virtualization dependency beyond what the SPA already has. */
export function PRList({
  prs,
  colorBy,
  selectedId,
  onSelect,
}: {
  prs: readonly DeliveryPR[];
  colorBy: ColorFacet;
  selectedId: string | undefined;
  onSelect: (id: string) => void;
}): ReactNode {
  const [sort, setSort] = useState<{ key: SortKey; direction: 1 | -1 }>({
    key: "merged",
    direction: -1,
  });
  const colorScale = useMemo(() => buildColorScale(prs, colorBy), [prs, colorBy]);
  const rows = useMemo(() => buildPRListRows(prs), [prs]);
  const sorted = useMemo(
    () => [...rows].sort((a, b) => compareRows(a, b, sort.key, sort.direction)),
    [rows, sort]
  );

  const toggleSort = (key: SortKey) =>
    setSort((current) =>
      current.key === key
        ? { key, direction: current.direction === 1 ? -1 : 1 }
        : { key, direction: -1 }
    );

  return (
    <div className="overflow-x-auto">
      <table className="min-w-[1024px] text-xs">
        <thead>
          <tr className={`border-b text-left ${borderDefault} ${textMutedOnCanvas}`}>
            {COLUMNS.map((column) => (
              <th className={`${column.className} px-2 py-1 font-medium`} key={column.key}>
                <button
                  className="inline-flex items-center gap-1"
                  onClick={() => toggleSort(column.key)}
                  type="button"
                >
                  {column.label}
                  {sort.key === column.key ? (sort.direction === 1 ? "\u2191" : "\u2193") : ""}
                </button>
              </th>
            ))}
            <th className="px-2 py-1 font-medium">Title</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((row) => (
            <tr
              className={`h-9 cursor-pointer border-b ${borderDefault} ${surfaceMutedHoverBg} ${
                selectedId === row.pr.id ? "font-semibold" : ""
              } ${textSecondaryOnCanvas}`}
              key={row.pr.id}
              onClick={() => onSelect(row.pr.id)}
            >
              <td className="px-2 py-1">
                {row.pr.merged_at === null ? "\u2014" : dateTime.format(new Date(row.pr.merged_at))}
              </td>
              <td className="px-2 py-1">
                <span
                  aria-hidden="true"
                  className="mr-1 inline-block size-2 rounded-full"
                  style={{ backgroundColor: colorScale.get(colorKeyFor(row.pr, colorBy)) }}
                />
                {row.prLabel}
              </td>
              <td className="px-2 py-1">{row.issueLabel}</td>
              <td className="px-2 py-1">{row.parentAgentLabel}</td>
              <td className="px-2 py-1">{row.pr.author}</td>
              <td className="px-2 py-1 text-right">{row.size ?? "\u2014"}</td>
              <td className="px-2 py-1">
                {row.deployedLabel}
                {row.deployedMs !== undefined
                  ? ` \u00b7 ${dateTime.format(new Date(row.deployedMs))}`
                  : ""}
              </td>
              <td className="px-2 py-1 text-right">{formatMinutes(row.leadTimeMinutes)}</td>
              <td className="px-2 py-1">{row.pr.rework ? "rework" : ""}</td>
              <td className="max-w-md truncate px-2 py-1" title={row.pr.title}>
                {row.pr.title}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {sorted.length === 0 ? (
        <p className={`p-3 ${textMutedOnCanvas}`}>No PRs match the current filters.</p>
      ) : null}
    </div>
  );
}
