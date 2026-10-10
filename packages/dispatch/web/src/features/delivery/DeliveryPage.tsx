import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";

import { type DeliveryTimelineOptions, isDeliveryNotConfigured } from "../../api/client";
import { deliveryTimelineQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { useRepeatableSearchParams, useSearchParamsUpdate } from "../../lib/url-array-params";
import {
  borderStrong,
  dragHandleBg,
  linkText,
  surfaceMutedBg,
  switchOffBg,
  switchOnBg,
  switchThumbBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { DeliverySettingsForm } from "./DeliverySettingsForm";
import { DrillDown } from "./DrillDown";
import { FacetPanel } from "./FacetPanel";
import {
  buildColorScale,
  COLOR_COUNT_KEYS,
  COLOR_FACET_OPTIONS,
  type ColorFacet,
} from "./lib/colorScale";
import {
  type DeployedFacet,
  FACET_KEYS,
  FACET_PARAMS,
  type Filters,
  type ReworkFacet,
} from "./lib/facets";
import { PRList } from "./PRList";
import { SourceFreshness } from "./SourceFreshness";
import { Timeline, type TimelineSelection } from "./Timeline";

/** The window the page reads when its URL names none: the prototype's 28 days. */
const DEFAULT_WINDOW_MS = 28 * 24 * 60 * 60 * 1000;

/** How long the search waits after the last keystroke before the server is asked again. */
const SEARCH_DEBOUNCE_MS = 250;

const COLOR_FACETS = COLOR_FACET_OPTIONS.map((option) => option.value);

/** The colour-by facet the page uses when its URL names none, as the prototype does. */
const DEFAULT_COLOR_BY: ColorFacet = "repo";

type Mode = "timeline" | "list";

/** How the filtered PRs are shown: dots on the timeline or a sortable list, the facet the merges
 *  are coloured by, and whether the timeline splits them into one column per value. */
interface DeliveryView {
  colorBy: ColorFacet;
  lanes: boolean;
  mode: Mode;
}

/** A sub-window of the read window a brush drag set (`ws`/`we`): it narrows what the chart, the
 *  list and the count show without a new read, so the facets keep counting the whole window. */
export interface BrushWindow {
  start: string;
  end: string;
}

interface DeliveryUrlState {
  from: string;
  to: string;
  filters: Filters;
  view: DeliveryView;
  brush: BrushWindow | null;
}

/** The delivery page's full state, read from and written to the URL — the same pattern
 *  `features/project/issue-filters.ts`'s `useIssueFilters` uses: repeatable facet params through
 *  the shared `useRepeatableSearchParams`, one `update` (`useSearchParamsUpdate`) funnelling every
 *  write through `setSearchParams(..., {replace: true})`. `filters` is memoized on the URL's own
 *  identity, so a render with an unchanged URL hands `deliveryTimelineQuery` the same options. */
function useDeliveryUrlState(): [DeliveryUrlState, (next: DeliveryUrlState) => void] {
  const [searchParams] = useSearchParams();
  const defaultWindowRef = useRef<{ from: string; to: string } | undefined>(undefined);
  // Frozen at first render: computing "now" fresh on every render would mint a new ISO string
  // (and so a new deliveryTimelineQuery cache key) on every render whenever the URL carries no
  // explicit from/to, refetching in a tight loop instead of once.
  if (defaultWindowRef.current === undefined) {
    const nowMs = Date.now();
    defaultWindowRef.current = {
      from: new Date(nowMs - DEFAULT_WINDOW_MS).toISOString(),
      to: new Date(nowMs).toISOString(),
    };
  }
  const from = searchParams.get("from") ?? defaultWindowRef.current.from;
  const to = searchParams.get("to") ?? defaultWindowRef.current.to;
  const search = searchParams.get("q") ?? "";
  const colorByParam = searchParams.get("colorBy");
  const colorBy = COLOR_FACETS.find((facet) => facet === colorByParam) ?? DEFAULT_COLOR_BY;
  const lanes = searchParams.get("lanes") === "1";
  const mode: Mode = searchParams.get("mode") === "list" ? "list" : "timeline";
  const view = useMemo(() => ({ colorBy, lanes, mode }), [colorBy, lanes, mode]);
  const ws = searchParams.get("ws");
  const we = searchParams.get("we");
  const brush = useMemo(
    () => (ws === null || we === null ? null : { start: ws, end: we }),
    [ws, we]
  );
  const arrayParams = useRepeatableSearchParams(FACET_KEYS);
  const filters: Filters = useMemo(
    () => ({
      ...arrayParams,
      rework: arrayParams.rework as ReworkFacet[],
      deployed: arrayParams.deployed as DeployedFacet[],
      search,
    }),
    [arrayParams, search]
  );

  const update = useSearchParamsUpdate();
  const setState = useCallback(
    (next: DeliveryUrlState) => {
      update((params) => {
        params.set("from", next.from);
        params.set("to", next.to);
        for (const key of FACET_KEYS) {
          params.delete(key);
          for (const value of next.filters[key]) params.append(key, value);
        }
        if (next.filters.search.trim() === "") params.delete("q");
        else params.set("q", next.filters.search);
        if (next.view.colorBy === DEFAULT_COLOR_BY) params.delete("colorBy");
        else params.set("colorBy", next.view.colorBy);
        if (next.view.lanes) params.set("lanes", "1");
        else params.delete("lanes");
        if (next.view.mode === "list") params.set("mode", "list");
        else params.delete("mode");
        if (next.brush === null) {
          params.delete("ws");
          params.delete("we");
        } else {
          params.set("ws", next.brush.start);
          params.set("we", next.brush.end);
        }
      });
    },
    [update]
  );

  return [{ from, to, filters, view, brush }, setState];
}

/** `value`, once it has held still for `delayMs`. */
function useSettled<T>(value: T, delayMs: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = window.setTimeout(() => setSettled(value), delayMs);
    return () => window.clearTimeout(timer);
  }, [value, delayMs]);
  return settled;
}

/** The Timeline | List segmented toggle. */
function ModeToggle({ mode, onChange }: { mode: Mode; onChange: (mode: Mode) => void }): ReactNode {
  const item = (value: Mode, label: string) => (
    <button
      aria-pressed={mode === value}
      className={`h-8 px-3 text-sm font-medium first:rounded-l-md last:rounded-r-md ${
        mode === value ? `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}` : textSecondaryOnCanvas
      }`}
      onClick={() => onChange(value)}
      type="button"
    >
      {label}
    </button>
  );
  return (
    <fieldset aria-label="View" className={`mr-4 flex rounded-md border ${borderStrong}`}>
      {item("timeline", "Timeline")}
      <span aria-hidden="true" className={`w-px ${dragHandleBg}`} />
      {item("list", "List")}
    </fieldset>
  );
}

/** The Swimlanes switch. */
function LanesSwitch({
  checked,
  onChange,
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
}): ReactNode {
  return (
    <label className="ml-4 flex cursor-pointer items-center gap-2">
      <button
        aria-checked={checked}
        className={`inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors ${checked ? switchOnBg : switchOffBg}`}
        onClick={() => onChange(!checked)}
        role="switch"
        type="button"
      >
        <span
          className={`block h-4 w-4 rounded-full shadow-lg transition-transform ${switchThumbBg} ${checked ? "translate-x-4.5" : "translate-x-0.5"}`}
        />
      </button>
      <span className={textPrimaryOnCanvas}>Swimlanes</span>
    </label>
  );
}

/** `/delivery`: successful production deploys, pipeline failures, and merged PRs labelled by
 *  agent session, Dispatch issue, priority, and architectural component, as the prototype's
 *  timeline view shows them. Until the timeline's configuration is set, the server answers
 *  `DELIVERY_NOT_CONFIGURED` and the page shows the settings form in the timeline's place; a save
 *  refetches the timeline, which then renders. */
export function DeliveryPage(): ReactNode {
  useDocumentTitle("Delivery · Dispatch");
  const [state, setState] = useDeliveryUrlState();
  const [selection, setSelection] = useState<TimelineSelection | null>(null);
  const search = useSettled(state.filters.search.trim(), SEARCH_DEBOUNCE_MS);

  // Every active facet and the search, sent to the server: `query.data.prs` is already the
  // filtered list. Memoized so a render with an unchanged URL hands `deliveryTimelineQuery` back
  // the same options object.
  const timelineOptions: DeliveryTimelineOptions = useMemo(() => {
    const options: DeliveryTimelineOptions = { from: state.from, to: state.to, q: search };
    const facets: Record<string, readonly string[]> = {};
    for (const key of FACET_KEYS) facets[FACET_PARAMS[key]] = state.filters[key];
    return { ...options, ...facets };
  }, [state.from, state.to, state.filters, search]);
  const query = useQuery(deliveryTimelineQuery(timelineOptions));
  const data = query.data;
  // When the answer on screen was read. While a facet change refetches, the previous answer stays
  // on screen (`keepPreviousData`) and the new query reports `dataUpdatedAt` 0, so this keeps the
  // last time an answer arrived instead of reading that as the epoch. The response carries no
  // generated time of its own.
  const answeredAtRef = useRef(0);
  if (query.dataUpdatedAt > 0) answeredAtRef.current = query.dataUpdatedAt;
  const answeredAt = answeredAtRef.current;

  const activeWindow = useMemo(
    () =>
      state.brush ?? (data === undefined ? null : { start: data.window.from, end: data.window.to }),
    [state.brush, data]
  );
  const shownPRs = useMemo(() => {
    const prs = data?.prs ?? [];
    if (state.brush === null) return prs;
    const startMs = Date.parse(state.brush.start);
    const endMs = Date.parse(state.brush.end);
    return prs.filter((pr) => {
      if (pr.merged_at === null) return false;
      const mergedMs = Date.parse(pr.merged_at);
      return mergedMs >= startMs && mergedMs <= endMs;
    });
  }, [data, state.brush]);
  // The waiting line's population, whatever the brush: the pull requests still waiting at the
  // read window's start, and every tracked merge of the read window. The brush narrows the line's
  // time range, never what it counts, so a merge before the brush that has not shipped holds the
  // line up across it.
  const waitingMerges = useMemo(
    () => [
      ...(data?.waiting ?? []),
      ...(data?.prs ?? []).filter((pr) => pr.deployed_status !== "not_tracked"),
    ],
    [data]
  );
  const colorScale = useMemo(
    () => buildColorScale(data?.color_counts[COLOR_COUNT_KEYS[state.view.colorBy]] ?? {}),
    [data, state.view.colorBy]
  );
  const allRepos = useMemo(() => Object.keys(data?.color_counts.repo ?? {}), [data]);

  const drillDownSelection = useMemo((): TimelineSelection | null => {
    if (selection === null) return null;
    if (selection.kind !== "pr") return selection;
    return data?.prs.some((pr) => pr.id === selection.id) ? selection : null;
  }, [selection, data]);

  const setView = (view: DeliveryView) => setState({ ...state, view });

  // Whether the page shows the setup form: the last settled timeline read's answer, held while a
  // read is in flight. A refetch with nothing cached clears `query.error` the moment it starts
  // (the window regaining focus, the event stream's reconnect refresh), so a body read live from
  // the query would unmount the form, and the draft typed into it, on every tab switch. A read
  // that settles on anything but `DELIVERY_NOT_CONFIGURED` shows that instead: the timeline a
  // save brings, or a failure with Retry. Set during render (React's pattern for state derived
  // from props), so the render that settles already shows the new body.
  const [notConfigured, setNotConfigured] = useState(false);
  if (query.fetchStatus === "idle" && isDeliveryNotConfigured(query.error) !== notConfigured) {
    setNotConfigured(!notConfigured);
  }
  return (
    <section className="flex flex-col gap-3 xl:h-[calc(100dvh-3rem)]">
      <header className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h1 className={`text-xl font-bold ${textPrimaryOnCanvas}`}>Delivery timeline</h1>
        {data === undefined ? null : (
          <div className={`text-xs ${textMutedOnCanvas}`}>
            Generated {new Date(answeredAt).toLocaleString()} · window{" "}
            {new Date(data.window.from).toLocaleDateString()}–
            {new Date(data.window.to).toLocaleDateString()}
            {state.brush === null ? null : (
              <button
                className={`ml-3 underline ${linkText}`}
                onClick={() => setState({ ...state, brush: null })}
                type="button"
              >
                clear brush window
              </button>
            )}
          </div>
        )}
      </header>

      {notConfigured ? (
        <section aria-labelledby="delivery-setup-heading" className="max-w-4xl">
          <h2
            className={`text-base font-semibold ${textPrimaryOnCanvas}`}
            id="delivery-setup-heading"
          >
            Set up the delivery timeline
          </h2>
          <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
            Once set, this page shows the merged pull requests of the authors you name, the
            production deploys that shipped them, and the pipeline failures in between.
          </p>
          <DeliverySettingsForm initial={null} />
        </section>
      ) : (
        <>
          {data === undefined ? null : (
            <SourceFreshness freshness={data.freshness} readAtMs={answeredAt} />
          )}

          {query.isPending ? <p className={textMutedOnCanvas}>Loading delivery timeline…</p> : null}
          {query.isError ? (
            <QueryError
              message="Couldn't load the delivery timeline."
              onRetry={() => void query.refetch()}
            />
          ) : null}

          {data === undefined || activeWindow === null ? null : (
            <>
              <div className="flex flex-wrap items-center gap-2 text-sm">
                <ModeToggle
                  mode={state.view.mode}
                  onChange={(mode) => setView({ ...state.view, mode })}
                />
                <label className={`flex items-center gap-2 ${textMutedOnCanvas}`}>
                  Color merges by
                  <select
                    className={`rounded border px-2 py-1 ${surfaceMutedBg} ${borderStrong} ${textPrimaryOnCanvas}`}
                    onChange={(event) => {
                      const colorBy = COLOR_FACETS.find((facet) => facet === event.target.value);
                      if (colorBy !== undefined) setView({ ...state.view, colorBy });
                    }}
                    value={state.view.colorBy}
                  >
                    {COLOR_FACET_OPTIONS.map((option) => (
                      <option key={option.value} value={option.value}>
                        {option.label}
                      </option>
                    ))}
                  </select>
                </label>
                {state.view.mode === "timeline" ? (
                  <LanesSwitch
                    checked={state.view.lanes}
                    onChange={(lanes) => setView({ ...state.view, lanes })}
                  />
                ) : null}
                <span className={`ml-4 ${textMutedOnCanvas}`}>
                  {shownPRs.length} PRs in current filter/window
                </span>
              </div>

              <div className="flex min-h-0 flex-1 flex-col gap-4 xl:flex-row">
                <div className="shrink-0 xl:w-56 xl:overflow-y-auto">
                  <FacetPanel
                    data={data}
                    filters={state.filters}
                    onChange={(filters) => setState({ ...state, filters })}
                  />
                </div>
                <div className="flex h-[75dvh] min-h-[28rem] min-w-0 flex-col xl:h-auto xl:flex-1">
                  {state.view.mode === "timeline" ? (
                    <Timeline
                      colorBy={state.view.colorBy}
                      colorScale={colorScale}
                      components={data.components}
                      filters={state.filters}
                      lanes={state.view.lanes}
                      onBrush={(brush) => setState({ ...state, brush })}
                      onSelect={setSelection}
                      prs={shownPRs}
                      runs={data.runs}
                      waiting={waitingMerges}
                      window={activeWindow}
                    />
                  ) : (
                    <PRList
                      allRepos={allRepos}
                      colorBy={state.view.colorBy}
                      colorScale={colorScale}
                      onSelect={(id) => setSelection({ kind: "pr", id })}
                      onSelectRun={(id) => setSelection({ kind: "deploy", id })}
                      prs={shownPRs}
                      selectedId={selection?.kind === "pr" ? selection.id : undefined}
                    />
                  )}
                </div>
                <DrillDown
                  components={data.components}
                  onClose={() => setSelection(null)}
                  prs={data.prs}
                  runs={data.runs}
                  selection={drillDownSelection}
                />
              </div>
            </>
          )}
        </>
      )}
    </section>
  );
}
