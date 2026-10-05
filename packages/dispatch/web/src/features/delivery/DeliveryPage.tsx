import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";

import { deliveryTimelineQuery } from "../../api/queries";
import { QueryError } from "../../components/QueryError";
import { bgTransparent, textMutedOnCanvas, textPrimaryOnCanvas } from "../../theme/classes";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { DrillDown } from "./DrillDown";
import { FacetPanel } from "./FacetPanel";
import type { ColorFacet } from "./lib/colorScale";
import { type DeployedFacet, type Filters, filterPRs, type ReworkFacet } from "./lib/facets";
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
 *  `features/project/issue-filters.ts`'s `useIssueFilters` uses: repeatable params via
 *  `searchParams.getAll`, one `update` helper funnelling every write through
 *  `setSearchParams(..., {replace: true})`. */
function useDeliveryUrlState(): [DeliveryUrlState, (next: DeliveryUrlState) => void] {
  const [searchParams, setSearchParams] = useSearchParams();
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
  const filters: Filters = {
    repo: searchParams.getAll("repo"),
    parentAgent: searchParams.getAll("parentAgent"),
    session: searchParams.getAll("session"),
    issue: searchParams.getAll("issue"),
    author: searchParams.getAll("author"),
    rework: searchParams.getAll("rework") as ReworkFacet[],
    deployed: searchParams.getAll("deployed") as DeployedFacet[],
    search: searchParams.get("q") ?? "",
  };
  const priority = searchParams.getAll("priority");
  const component = searchParams.getAll("component");

  const setState = useCallback(
    (next: DeliveryUrlState) => {
      setSearchParams(
        (current) => {
          const params = new URLSearchParams(current.toString());
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
          return params;
        },
        { replace: true }
      );
    },
    [setSearchParams]
  );

  return [{ from, to, mode, filters, priority, component }, setState];
}

const COLOR_BY_OPTIONS: { value: ColorFacet; label: string }[] = [
  { value: "repo", label: "Repository" },
  { value: "author", label: "Author" },
  { value: "parentAgent", label: "Parent agent" },
];

/** `/delivery`: successful production deploys, pipeline failures, and merged PRs labelled by
 *  agent session, Dispatch issue, priority, and architectural component — LEGION-567 slice 1,
 *  ported from the local prototype at `~/proto/delivery-timeline`. */
export function DeliveryPage(): ReactNode {
  useDocumentTitle("Delivery · Dispatch");
  const [state, setState] = useDeliveryUrlState();
  const [colorBy, setColorBy] = useState<ColorFacet>("repo");
  const [selection, setSelection] = useState<TimelineSelection | null>(null);

  const query = useQuery(
    deliveryTimelineQuery({
      from: state.from,
      to: state.to,
      priority: state.priority,
      component: state.component,
    })
  );

  const filteredPRs = useMemo(
    () => (query.data === undefined ? [] : filterPRs(query.data.prs, state.filters)),
    [query.data, state.filters]
  );

  const drillDownSelection = useMemo((): TimelineSelection | null => {
    if (selection === null) return null;
    if (selection.kind !== "pr") return selection;
    return filteredPRs.some((pr) => pr.id === selection.id) ? selection : null;
  }, [selection, filteredPRs]);

  return (
    <section className="flex h-full flex-col gap-4">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h1 className={`text-lg font-semibold ${textPrimaryOnCanvas}`}>Delivery</h1>
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
      </header>

      {query.data === undefined ? null : <SourceFreshness freshness={query.data.freshness} />}

      <FacetPanel
        component={state.component}
        filters={state.filters}
        onChange={(filters) => setState({ ...state, filters })}
        onComponentChange={(component) => setState({ ...state, component })}
        onPriorityChange={(priority) => setState({ ...state, priority })}
        prs={query.data?.prs ?? []}
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
                prs={filteredPRs}
                runs={query.data.runs}
              />
            ) : (
              <PRList
                colorBy={colorBy}
                onSelect={(id) => setSelection({ kind: "pr", id })}
                prs={filteredPRs}
                selectedId={selection?.kind === "pr" ? selection.id : undefined}
              />
            )}
          </div>
          <DrillDown
            onClose={() => setSelection(null)}
            prs={filteredPRs}
            runs={query.data.runs}
            selection={drillDownSelection}
          />
        </div>
      )}
    </section>
  );
}
