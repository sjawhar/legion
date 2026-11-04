// The one component every chart that draws a measure over a range goes through (INFRA-1077): the
// measures panel's KPI cards and the Pipeline cards. It wraps `Sparkbars` and `SpreadBars` rather
// than replacing them, so `lib/barScale.ts` stays the one scale and `theme/classes.ts`'s
// `chartBar.*` the one palette, and adds the two controls the spec gives every such chart:
//
//   - Graph or table. The table holds every number its graph draws: a run of bars is one column,
//     a median-and-p90 spread is three (the median, the p90 and the results they came from), a
//     target line is named in the caption, and a day with nothing measured is a dash, never a
//     zero.
//   - Compare. The reader picks a second range, whose default is the range immediately before
//     the window, of the same length (`precedingRange`). The graph draws it under the window's
//     own bars and the table puts the two side by side, each pair with the change between them.
//
// The compare range belongs to the page, not to the chart: the chart names it and the page reads
// it as a second request, one for the window and one for the comparison, each inside the server's
// 84-day cap, which `customRange` holds a reader's own range to. A page that reads no second
// range passes no `compare` and the chart shows the graph-or-table control alone.
//
// `Timeline.tsx` is not one of these charts: it draws individual pull requests as points, keeps
// its own `ModeToggle` (Timeline <-> PRList) and its brush, and two overlaid ranges of a scatter
// is not a comparison a reader can read (the plan's departure S-4).
import { Fragment, type ReactNode, useId, useMemo, useState } from "react";

import {
  borderDefault,
  borderStrong,
  dangerText,
  surfaceBg,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { customRange, DAY_MS, lastUtcDate, utcDate, type WindowChoice } from "./lib/window";
import { type SegmentedOption, SegmentedToggle } from "./SegmentedToggle";
import { type SparkbarPoint, Sparkbars } from "./Sparkbars";
import { type SpreadBarDay, SpreadBars } from "./SpreadBars";
import { UtcRangeFields } from "./UtcRangeFields";

/** A chart's range: the custom member of the page's own `WindowChoice` (`lib/window.ts`), since
 *  a chart draws the range its answer resolved and a comparison names two exact ends, never a
 *  rolling preset. `liveWindow` turns one into the window a page reads it with. */
export type ChartRange = Extract<WindowChoice, { kind: "custom" }>;

/** A run of per-day numbers, as `Sparkbars` takes them; `label` heads its table column. */
interface ChartSparkSeries {
  readonly kind: "spark";
  readonly label: string;
  readonly points: SparkbarPoint[];
  readonly target?: number;
  readonly max: number;
  readonly met?: (value: number) => boolean;
  readonly format: (value: number) => string;
}

/** A run of per-day medians and p90s, as `SpreadBars` takes them; `label` heads its two duration
 *  columns and `noun` the column counting what they came from. */
interface ChartSpreadSeries {
  readonly kind: "spread";
  readonly label: string;
  readonly days: readonly SpreadBarDay[];
  readonly target?: number;
  readonly noun: string;
  readonly format: (minutes: number) => string;
}

/** One run of numbers a chart draws. */
export type ChartSeries = ChartSparkSeries | ChartSpreadSeries;

/** The comparison a page offers a chart: the range it reads beside the window, how the reader
 *  names another, and that range's series once its read lands. */
export interface ChartCompare {
  /** The range the page is reading beside the window, or null while nothing is compared. */
  readonly range: ChartRange | null;
  /** The range the reader picked; null turns the comparison off. */
  readonly onRange: (next: ChartRange | null) => void;
  /** The compare range's series, in the chart's own order; undefined while its read is out. */
  readonly series: readonly ChartSeries[] | undefined;
}

/** Where a host's comparison comes from: the range it reads beside the window, how the reader
 *  names another, and that range's own answer once its read lands. A section holds one of these
 *  and `chartCompare` turns it into each chart's `ChartCompare`. */
export interface ChartCompareSource<T> {
  /** The range the host is reading beside the window, or null while nothing is compared. */
  readonly range: ChartRange | null;
  /** The range the reader picked; null turns the comparison off. */
  readonly onRange: (next: ChartRange | null) => void;
  /** The compare range's answer; undefined while its read is out. */
  readonly data: T | undefined;
}

/** One chart's comparison: the host's range and callback, with `series` as that chart reads the
 *  compare range's answer. A host that reads no second range has no source and the chart shows
 *  its graph-or-table control alone; a chart whose series the answer does not hold (a card the
 *  compare answer has nothing for) compares nothing. */
export function chartCompare<T>(
  source: ChartCompareSource<T> | undefined,
  series: readonly ChartSeries[] | null | undefined
): ChartCompare | undefined {
  if (source === undefined) return undefined;
  return {
    onRange: source.onRange,
    range: source.range,
    series: source.data === undefined ? undefined : (series ?? undefined),
  };
}

/** What the Delivery page gives each of its sections: its one window and its one compare range,
 *  so every chart on the page follows one window control and one comparison. */
export interface DeliverySectionProps {
  /** The range the page reads beside its window, or null while nothing is compared. */
  readonly compare: ChartRange | null;
  /** The range a chart's compare control named; null turns the comparison off. */
  readonly onCompare: (next: ChartRange | null) => void;
  /** The page's window, which every section follows. */
  readonly window: WindowChoice;
}

/** What one row of a chart's table is, where it is not a UTC day: the heading of the table's
 *  first column, and the caption's own words for the run the series draw ("<label> by <over>").
 *  The Legion section's cards draw a measure over the window by issue, by worker type and by UTC
 *  week; every other chart on the page draws one per UTC day. */
export interface ChartRows {
  readonly header: string;
  readonly over: string;
}

/** What a run of per-day bars is: the rows every chart has unless it names its own. */
const UTC_DAYS: ChartRows = { header: "Day", over: "UTC day" };

export interface ChartProps {
  /** What the chart draws, in the reader's words: it names both controls and heads the table. */
  readonly label: string;
  /** The range the series cover, as the answer resolved it. */
  readonly range: ChartRange;
  /** The series drawn, in order; the table holds every number all of them draw. */
  readonly series: readonly ChartSeries[];
  /** The comparison, from the page that reads its range; omitted where none is read. */
  readonly compare?: ChartCompare;
  /** What one row of its table is, where it is not a UTC day. */
  readonly rows?: ChartRows;
}

/** What a day with nothing measured reads as: never a zero. */
const NOTHING = "\u2014";

/** What a number whose read is still out reads as, as the measures panel's numbers do. */
export const PENDING = "\u2026";

type ChartMode = "graph" | "table";

const MODES: readonly SegmentedOption<ChartMode>[] = [
  { value: "graph", label: "Graph" },
  { value: "table", label: "Table" },
];

/** A small control beside a chart: the compare picker here, and the Legion section's project
 *  picker, so one shape serves every control the page's charts sit under. */
export const fieldClass = `rounded border px-1.5 py-0.5 text-xs ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`;

const noteClass = `text-[11px] ${textMutedOnSurface}`;

/** The range immediately before `range`, of the same length: what a comparison starts from. It
 *  is as long as a range the server already answered, so it is inside the server's cap too. */
export function precedingRange(range: ChartRange): ChartRange {
  const from = Date.parse(range.from);
  const to = Date.parse(range.to);
  return {
    kind: "custom",
    from: new Date(from - (to - from)).toISOString(),
    to: new Date(from).toISOString(),
  };
}

/** The range a run of whole UTC days covers, read as the window control reads a reader's two
 *  dates: the first day's midnight to the midnight after the last. */
export function daysRange(first: string, last: string): ChartRange {
  const range = customRange(first, last);
  if ("refused" in range) throw new Error(`a chart's days are no range: ${range.refused}`);
  return { kind: "custom", ...range };
}

/** Whether two ranges name the same two instants. */
function sameRange(one: ChartRange, other: ChartRange): boolean {
  return (
    Date.parse(one.from) === Date.parse(other.from) && Date.parse(one.to) === Date.parse(other.to)
  );
}

/** A range in the reader's words: the UTC days it covers. A range ending at a midnight ends on
 *  the day before, as `lastUtcDate` reads a whole-day range's exclusive end; a rolling preset,
 *  whose end is the present, ends on the day it is in, which is the last day the bars draw. */
function rangeLabel(range: ChartRange): string {
  const to = Date.parse(range.to);
  return `${utcDate(range.from)} to ${to % DAY_MS === 0 ? lastUtcDate(range.to) : utcDate(range.to)}`;
}

/** How long a range is, in the reader's words; a brushed window may be hours, which no whole
 *  number of days names. */
function spanLabel(range: ChartRange): string {
  const days = (Date.parse(range.to) - Date.parse(range.from)) / DAY_MS;
  if (!Number.isInteger(days) || days < 1) return "period";
  return days === 1 ? "day" : `${days} days`;
}

/** One number the graph draws, as the table holds it. */
interface ChartColumn {
  readonly header: string;
  readonly format: (value: number) => string;
}

/** One day's cell in every column, in the columns' order; null is nothing measured. */
interface ChartRow {
  readonly day: string;
  readonly partial: boolean;
  readonly values: readonly (number | null)[];
}

interface ChartTable {
  readonly columns: readonly ChartColumn[];
  readonly rows: readonly ChartRow[];
}

const asCount = (value: number): string => `${value}`;

/** Every number a series set draws, by day: a run of bars fills one column, a spread fills its
 *  median, its p90 and the results they came from, and a day one series does not reach is null
 *  there. Days keep the order the series hold them in, oldest first. */
function tableOf(series: readonly ChartSeries[]): ChartTable {
  const columns: ChartColumn[] = [];
  for (const one of series) {
    if (one.kind === "spark") {
      columns.push({ format: one.format, header: one.label });
      continue;
    }
    columns.push({ format: one.format, header: `${one.label} median` });
    columns.push({ format: one.format, header: `${one.label} p90` });
    // The count the spread's two durations came from, headed by what the series counts.
    columns.push({
      format: asCount,
      header: one.noun.charAt(0).toUpperCase() + one.noun.slice(1),
    });
  }
  const order: string[] = [];
  const cells = new Map<string, (number | null)[]>();
  const partial = new Set<string>();
  const row = (day: string): (number | null)[] => {
    const held = cells.get(day);
    if (held !== undefined) return held;
    const fresh: (number | null)[] = columns.map(() => null);
    cells.set(day, fresh);
    order.push(day);
    return fresh;
  };
  let at = 0;
  for (const one of series) {
    if (one.kind === "spark") {
      for (const point of one.points) {
        row(point.label)[at] = point.value;
        if (point.partial) partial.add(point.label);
      }
      at += 1;
      continue;
    }
    for (const day of one.days) {
      const values = row(day.day);
      values[at] = day.median_minutes;
      values[at + 1] = day.p90_minutes;
      values[at + 2] = day.n;
      if (day.partial) partial.add(day.day);
    }
    at += 3;
  }
  return {
    columns,
    rows: order.map((day) => ({
      day,
      partial: partial.has(day),
      values: cells.get(day) ?? [],
    })),
  };
}

/** A number as the table reads it: nothing measured where there is none, and an ellipsis where
 *  the read that would hold it is still out. */
function numberText(
  value: number | null | undefined,
  format: (value: number) => string,
  pending: boolean
): string {
  if (pending) return PENDING;
  return value === null || value === undefined ? NOTHING : format(value);
}

/** The change from the compare range's number to the window's, signed. */
function changeText(
  value: number | null | undefined,
  before: number | null | undefined,
  format: (value: number) => string,
  pending: boolean
): string {
  if (pending) return PENDING;
  if (value === null || value === undefined || before === null || before === undefined) {
    return NOTHING;
  }
  const change = value - before;
  if (change > 0) return `+${format(change)}`;
  if (change < 0) return `-${format(-change)}`;
  return format(0);
}

/** What the reader compares the window with, once they have picked a range. */
interface Comparison {
  readonly range: ChartRange;
  readonly series: readonly ChartSeries[] | undefined;
}

/** One range's bars under the days they cover; `series` is undefined while its read is out. */
function RangeGraph({
  compare,
  label,
  series,
}: {
  compare: boolean;
  label: string;
  series: ChartSeries | undefined;
}): ReactNode {
  return (
    <div data-chart-range={compare ? "compare" : "window"}>
      <div className={noteClass}>{compare ? `Compare ${label}` : label}</div>
      {series === undefined ? (
        <p className={noteClass}>{PENDING}</p>
      ) : series.kind === "spark" ? (
        <Sparkbars
          format={series.format}
          max={series.max}
          met={series.met}
          points={series.points}
          target={series.target}
        />
      ) : (
        <SpreadBars
          days={series.days}
          format={series.format}
          noun={series.noun}
          target={series.target}
        />
      )}
    </div>
  );
}

/** The chart itself: every series' bars, and with a comparison the second range's under each. */
function Graph({
  against,
  range,
  series,
}: {
  against: Comparison | null;
  range: ChartRange;
  series: readonly ChartSeries[];
}): ReactNode {
  return (
    <>
      {series.map((one, index) =>
        against === null ? (
          <RangeGraph compare={false} key={one.label} label={rangeLabel(range)} series={one} />
        ) : (
          <Fragment key={one.label}>
            <RangeGraph compare={false} label={rangeLabel(range)} series={one} />
            <RangeGraph
              compare={true}
              label={rangeLabel(against.range)}
              series={against.series?.[index]}
            />
          </Fragment>
        )
      )}
    </>
  );
}

/** Every number the graph draws, in a table: one row a day (or whatever `naming` says a row is),
 *  one column each number, and with a comparison the two ranges side by side with the change
 *  between them. */
function NumberTable({
  against,
  label,
  naming,
  series,
}: {
  against: Comparison | null;
  label: string;
  naming: ChartRows;
  series: readonly ChartSeries[];
}): ReactNode {
  // Every number the series draw, and every number the compare range's draw, built once per run
  // of series rather than on every render of the host: a live delta or a keystroke elsewhere on
  // the page re-renders the section around a chart whose numbers have not moved.
  const table = useMemo(() => tableOf(series), [series]);
  const pending = against !== null && against.series === undefined;
  const comparedSeries = against?.series;
  const compared = useMemo(
    () => (comparedSeries === undefined ? null : tableOf(comparedSeries)),
    [comparedSeries]
  );
  const comparing = against !== null;
  const notes = [`${label} by ${naming.over}`];
  if (against !== null) notes.push(`compared with ${rangeLabel(against.range)}`);
  for (const one of series) {
    if (one.target !== undefined) notes.push(`${one.label} target ${one.format(one.target)}`);
  }
  notes.push(`${NOTHING} is a ${naming.header.toLowerCase()} with nothing measured`);
  const rows = useMemo(
    () =>
      !comparing
        ? table.rows.map((row) => ({ before: undefined, row }))
        : Array.from(
            { length: Math.max(table.rows.length, compared?.rows.length ?? 0) },
            (_, index) => ({ before: compared?.rows[index], row: table.rows[index] })
          ),
    [comparing, compared, table]
  );
  return (
    <div className={`max-h-48 overflow-auto rounded border ${borderDefault}`}>
      <table className="w-full text-xs tabular-nums" data-chart-table={label}>
        <caption className={`px-2 py-1 text-left ${noteClass}`}>{`${notes.join(". ")}.`}</caption>
        <thead className={`sticky top-0 ${surfaceBg} ${textMutedOnSurface}`}>
          <tr>
            <th className="px-2 py-1 text-left font-normal">{naming.header}</th>
            {against === null ? null : (
              <th className="px-2 py-1 text-left font-normal">{`Compare ${naming.header.toLowerCase()}`}</th>
            )}
            {table.columns.map((column) => (
              <Fragment key={column.header}>
                <th className="px-2 py-1 text-right font-normal">{column.header}</th>
                {against === null ? null : (
                  <>
                    <th className="px-2 py-1 text-right font-normal">{`${column.header} compare`}</th>
                    <th className="px-2 py-1 text-right font-normal">{`${column.header} change`}</th>
                  </>
                )}
              </Fragment>
            ))}
          </tr>
        </thead>
        <tbody className={textMutedOnCanvas}>
          {rows.map(({ before, row }, index) => (
            <tr className={`border-t ${borderDefault}`} key={row?.day ?? before?.day ?? `${index}`}>
              <td className="px-2 py-0.5">
                {row === undefined ? NOTHING : `${row.day}${row.partial ? " (partial)" : ""}`}
              </td>
              {against === null ? null : (
                <td className="px-2 py-0.5">
                  {pending ? PENDING : before === undefined ? NOTHING : before.day}
                </td>
              )}
              {table.columns.map((column, at) => (
                <Fragment key={column.header}>
                  <td className="px-2 py-0.5 text-right">
                    {numberText(row?.values[at], column.format, false)}
                  </td>
                  {against === null ? null : (
                    <>
                      <td className="px-2 py-0.5 text-right">
                        {numberText(before?.values[at], column.format, pending)}
                      </td>
                      <td className="px-2 py-0.5 text-right">
                        {changeText(row?.values[at], before?.values[at], column.format, pending)}
                      </td>
                    </>
                  )}
                </Fragment>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** The compare control: no second range, the range just before the window, or one the reader
 *  names as two whole UTC days, which `customRange` holds to the server's 84-day cap. */
function CompareControl({
  compare,
  label,
  range,
}: {
  compare: ChartCompare;
  label: string;
  range: ChartRange;
}): ReactNode {
  const id = useId();
  const [picking, setPicking] = useState(false);
  const [draft, setDraft] = useState<{ from: string; to: string } | null>(null);
  const [refused, setRefused] = useState<string | null>(null);
  const preceding = precedingRange(range);
  // A range another chart on the page named: this control shows it as the reader's own, since
  // every chart offers the one comparison the page reads.
  const named =
    compare.range !== null && !sameRange(compare.range, preceding) ? compare.range : null;
  const chosen =
    compare.range === null
      ? picking
        ? "custom"
        : "none"
      : named === null
        ? "preceding"
        : "custom";
  const fromDate = draft?.from ?? (named === null ? "" : utcDate(named.from));
  const toDate = draft?.to ?? (named === null ? "" : lastUtcDate(named.to));

  const apply = () => {
    const picked = customRange(fromDate, toDate);
    if ("refused" in picked) {
      setRefused(picked.refused);
      return;
    }
    setRefused(null);
    compare.onRange({ kind: "custom", ...picked });
  };

  return (
    <div className={`flex flex-wrap items-center gap-1.5 text-xs ${textMutedOnCanvas}`}>
      <select
        aria-label={`${label} comparison`}
        className={fieldClass}
        data-chart-compare={label}
        onChange={(event) => {
          const next = event.target.value;
          if (next === "custom") {
            setPicking(true);
            return;
          }
          setPicking(false);
          setRefused(null);
          compare.onRange(next === "preceding" ? preceding : null);
        }}
        value={chosen}
      >
        <option value="none">No comparison</option>
        <option value="preceding">{`Previous ${spanLabel(range)}`}</option>
        <option value="custom">Custom range…</option>
      </select>
      {chosen === "custom" ? (
        <UtcRangeFields
          fieldClass={fieldClass}
          from={fromDate}
          idPrefix={id}
          onApply={apply}
          onFrom={(next) => setDraft({ from: next, to: toDate })}
          onTo={(next) => setDraft({ from: fromDate, to: next })}
          refusalClass={`basis-full ${dangerText}`}
          refused={refused}
          to={toDate}
        />
      ) : null}
    </div>
  );
}

/** One chart: its series as bars or as the table of every number they draw, under the two
 *  controls every chart of a measure over a range has. */
export function Chart({ compare, label, range, rows = UTC_DAYS, series }: ChartProps): ReactNode {
  const [mode, setMode] = useState<ChartMode>("graph");
  const against: Comparison | null =
    compare === undefined || compare.range === null
      ? null
      : { range: compare.range, series: compare.series };
  return (
    <div className="flex min-w-0 flex-col gap-1" data-chart={label}>
      {mode === "graph" ? (
        <Graph against={against} range={range} series={series} />
      ) : (
        <NumberTable against={against} label={label} naming={rows} series={series} />
      )}
      <div className="flex flex-wrap items-center gap-1.5">
        <SegmentedToggle label={`${label} view`} onChange={setMode} options={MODES} value={mode} />
        {compare === undefined ? null : (
          <CompareControl compare={compare} label={label} range={range} />
        )}
      </div>
    </div>
  );
}
