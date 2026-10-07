import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";

import { type DeliveryTimelineOptions, isDeliveryNotConfigured } from "../../api/client";
import { deliveryTimelineQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { useRepeatableSearchParams, useSearchParamsUpdate } from "../../lib/url-array-params";
import {
  bgTransparent,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { DeliverySettingsForm } from "./DeliverySettingsForm";
import { DrillDown } from "./DrillDown";
import { FacetPanel } from "./FacetPanel";
import type { ColorFacet } from "./lib/colorScale";
import type { DeployedFacet, Filters, ReworkFacet } from "./lib/facets";
import { PRList } from "./PRList";
import { SourceFreshness } from "./SourceFreshness";
import { Timeline, type TimelineSelection } from "./Timeline";

const ARRAY_KEYS = [
  "repo",
  "parentAgent",
  "session",
  "issue",
  "author",
  "rework",
  "deployed",
] as const satisfies readonly (keyof Omit<Filters, "search">)[];

/** Every repeatable facet param the page reads and writes in one URL round trip, including
 *  `priority`/`component` beside `ARRAY_KEYS`'s client-facet set — passed whole to
 *  `useRepeatableSearchParams`, whose memo depends on this array's own identity, so it is a
 *  module-level constant. */
const URL_ARRAY_KEYS = [...ARRAY_KEYS, "priority", "component"] as const;

const SEVEN_DAYS_MS = 7 * 24 * 60 * 60 * 1000;

type Mode = "timeline" | "list";

interface DeliveryUrlState {
  from: string;
  to: string;
  mode: Mode;
  filters: Filters;
  priority: string[];
  component: string[];
}

/** The delivery page's full state, read from and written to the URL — the same pattern
 *  `features/project/issue-filters.ts`'s `useIssueFilters` uses: repeatable params via the
 *  shared `useRepeatableSearchParams`, one `update` helper (`useSearchParamsUpdate`) funnelling
 *  every write through `setSearchParams(..., {replace: true})`. `filters`, `priority`, and
 *  `component` are memoized on the URL's own identity (`useRepeatableSearchParams`'s inner
 *  memo), so a render with an unchanged URL hands back the exact same arrays and `Filters`
 *  object rather than fresh ones every render — the identity `deliveryTimelineQuery`'s options
 *  (built from them, also memoized below) depend on to stay stable across renders the URL
 *  didn't change. */
function useDeliveryUrlState(): [DeliveryUrlState, (next: DeliveryUrlState) => void] {
  const [searchParams] = useSearchParams();
  const defaultWindowRef = useRef<{ from: string; to: string } | undefined>(undefined);
  // Frozen at first render: computing "now" fresh on every render would mint a new ISO string
  // (and so a new deliveryTimelineQuery cache key) on every render whenever the URL carries no
  // explicit from/to, refetching in a tight loop instead of once.
  if (defaultWindowRef.current === undefined) {
    const nowMs = Date.now();
    defaultWindowRef.current = {
      from: new Date(nowMs - SEVEN_DAYS_MS).toISOString(),
      to: new Date(nowMs).toISOString(),
    };
  }
  const from = searchParams.get("from") ?? defaultWindowRef.current.from;
  const to = searchParams.get("to") ?? defaultWindowRef.current.to;
  const mode: Mode = searchParams.get("mode") === "list" ? "list" : "timeline";
  const search = searchParams.get("q") ?? "";
  const arrayParams = useRepeatableSearchParams(URL_ARRAY_KEYS);
  const { priority, component } = arrayParams;
  const filters: Filters = useMemo(
    () => ({
      repo: arrayParams.repo,
      parentAgent: arrayParams.parentAgent,
      session: arrayParams.session,
      issue: arrayParams.issue,
      author: arrayParams.author,
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
        if (next.mode === "list") params.set("mode", "list");
        else params.delete("mode");
        for (const key of ARRAY_KEYS) {
          params.delete(key);
          for (const value of next.filters[key]) params.append(key, value);
        }
        if (next.filters.search.trim() === "") params.delete("q");
        else params.set("q", next.filters.search);
        params.delete("priority");
        for (const value of next.priority) params.append("priority", value);
        params.delete("component");
        for (const value of next.component) params.append("component", value);
      });
    },
    [update]
  );

  return [{ from, to, mode, filters, priority, component }, setState];
}

const COLOR_BY_OPTIONS: { value: ColorFacet; label: string }[] = [
  { value: "repo", label: "Repository" },
  { value: "author", label: "Author" },
  { value: "parentAgent", label: "Parent agent" },
];

/** `/delivery`: successful production deploys, pipeline failures, and merged PRs labelled by
 *  agent session, Dispatch issue, priority, and architectural component. Until the timeline's
 *  configuration is set, the server answers `DELIVERY_NOT_CONFIGURED` and the page shows the
 *  settings form in the timeline's place; a save refetches the timeline, which then renders. */
export function DeliveryPage(): ReactNode {
  useDocumentTitle("Delivery · Dispatch");
  const [state, setState] = useDeliveryUrlState();
  const [colorBy, setColorBy] = useState<ColorFacet>("repo");
  const [selection, setSelection] = useState<TimelineSelection | null>(null);

  // Every active facet, sent to the server (LEGION-567's plan, "API"): `query.data.prs` is
  // already the filtered list, so there is no client-side re-filter step here. Memoized so a
  // render with an unchanged URL hands `deliveryTimelineQuery` back the same options object,
  // not a fresh one the query key would otherwise have to re-hash every render.
  const timelineOptions: DeliveryTimelineOptions = useMemo(
    () => ({
      from: state.from,
      to: state.to,
      repo: state.filters.repo,
      parent_agent: state.filters.parentAgent,
      session: state.filters.session,
      issue: state.filters.issue,
      author: state.filters.author,
      rework: state.filters.rework,
      deployed: state.filters.deployed,
      priority: state.priority,
      component: state.component,
    }),
    [state.from, state.to, state.filters, state.priority, state.component]
  );
  const query = useQuery(deliveryTimelineQuery(timelineOptions));
  const prs = query.data?.prs ?? [];

  const drillDownSelection = useMemo((): TimelineSelection | null => {
    if (selection === null) return null;
    if (selection.kind !== "pr") return selection;
    return prs.some((pr) => pr.id === selection.id) ? selection : null;
  }, [selection, prs]);

  // Whether the page shows the setup form, latched: set once a timeline read settles on
  // `DELIVERY_NOT_CONFIGURED`, and cleared only once a read answers with a timeline, as the read
  // a save starts does. A refetch with nothing cached clears `query.error` the moment it starts
  // (the window regaining focus, the event stream's reconnect refresh), and one can fail on
  // something else, so a body read live from the query would unmount the form, and the draft
  // typed into it, on every tab switch. Set during render (React's pattern for state derived from
  // props), so the render that settles already shows the new body.
  const [notConfigured, setNotConfigured] = useState(false);
  if (
    notConfigured
      ? query.isSuccess
      : query.fetchStatus === "idle" && isDeliveryNotConfigured(query.error)
  ) {
    setNotConfigured(!notConfigured);
  }
  return (
    <section className="flex h-full flex-col gap-4">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h1 className={`text-lg font-semibold ${textPrimaryOnCanvas}`}>Delivery</h1>
        {notConfigured ? null : (
          <div className="flex items-center gap-2 text-sm">
            <label className={textMutedOnCanvas}>
              Color by
              <select
                className={`ml-2 rounded border px-2 py-1 ${bgTransparent}`}
                onChange={(event) => setColorBy(event.target.value as ColorFacet)}
                value={colorBy}
              >
                {COLOR_BY_OPTIONS.map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.label}
                  </option>
                ))}
              </select>
            </label>
            <button
              className="rounded border px-3 py-1"
              onClick={() =>
                setState({ ...state, mode: state.mode === "list" ? "timeline" : "list" })
              }
              type="button"
            >
              {state.mode === "list" ? "Show timeline" : "Show list"}
            </button>
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
          {query.data === undefined ? null : <SourceFreshness freshness={query.data.freshness} />}

          <FacetPanel
            component={state.component}
            filters={state.filters}
            onChange={(filters) => setState({ ...state, filters })}
            onComponentChange={(component) => setState({ ...state, component })}
            onPriorityChange={(priority) => setState({ ...state, priority })}
            prs={prs}
            priority={state.priority}
          />

          {query.isPending ? <p className={textMutedOnCanvas}>Loading delivery timeline…</p> : null}
          {query.isError ? (
            <QueryError
              message="Couldn't load the delivery timeline."
              onRetry={() => void query.refetch()}
            />
          ) : null}

          {query.data === undefined ? null : (
            <div className="flex min-h-0 flex-1 gap-4">
              <div className="min-w-0 flex-1">
                {state.mode === "timeline" ? (
                  <Timeline
                    colorBy={colorBy}
                    onBrush={(window) => setState({ ...state, from: window.from, to: window.to })}
                    onSelect={setSelection}
                    prs={prs}
                    runs={query.data.runs}
                  />
                ) : (
                  <PRList
                    colorBy={colorBy}
                    onSelect={(id) => setSelection({ kind: "pr", id })}
                    prs={prs}
                    selectedId={selection?.kind === "pr" ? selection.id : undefined}
                  />
                )}
              </div>
              <DrillDown
                onClose={() => setSelection(null)}
                prs={prs}
                runs={query.data.runs}
                selection={drillDownSelection}
              />
            </div>
          )}
        </>
      )}
    </section>
  );
}
