import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useRef } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { api } from "../../api/client";
import { architectureSourcesQuery } from "../../api/queries";
import type {
  ArchitectureTree,
  ArchitectureTreeComponent,
  ArchitectureTreeSource,
} from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import {
  badgeHigh,
  badgeLow,
  borderStrong,
  calloutDangerBg,
  calloutDangerBorder,
  card,
  cardHoverBorder,
  dangerText,
  focusVisibleRing,
  inlineWarningText,
  linkHoverText,
  linkText,
  progressFill,
  progressFillDone,
  progressTrack,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
  trustWarningBorder,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, roveFocus } from "../shell/roving";
import { AttachmentLists, type AttachmentView } from "./AttachmentLists";
import {
  type ComponentIndex,
  type ComponentTone,
  childComponents,
  componentPath,
  componentTone,
  distrustedSources,
  indexComponents,
  sourceState,
} from "./architecture-model";
import { ComponentDetails } from "./ComponentDetails";

const ROW_SELECTOR = "[data-component-row]";
const rowFocusRing = `outline-none focus-visible:ring-2 ${focusVisibleRing}`;

function rowAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, ROW_SELECTOR);
}

const VIEWS: Record<string, AttachmentView> = {
  none: "none",
  retired: "retired",
  unassigned: "unassigned",
};

type PaneView = AttachmentView | "no-work";

/** Rewrites the pane's URL state; `null` deletes a parameter, `undefined` leaves it alone. */
type UpdatePaneState = (changes: { component?: string | null; view?: PaneView | null }) => void;

/** The pane's URL state: `?component=<id>` (the level and the details), `?view=` (a side list
 *  or the no-tracked-work filter). */
function usePaneState(): {
  component: string | null;
  update: UpdatePaneState;
  view: PaneView | null;
} {
  const [searchParams, setSearchParams] = useSearchParams();
  const component = searchParams.get("component");
  const viewParam = searchParams.get("view");
  const view: PaneView | null =
    viewParam === "no-work" ? "no-work" : (VIEWS[viewParam ?? ""] ?? null);
  const update: UpdatePaneState = useCallback(
    (changes) => {
      setSearchParams((current) => {
        const next = new URLSearchParams(current.toString());
        for (const [name, value] of Object.entries(changes)) {
          if (value === null) {
            next.delete(name);
          } else if (value !== undefined) {
            next.set(name, value);
          }
        }
        return next;
      });
    },
    [setSearchParams]
  );
  return { component, update, view };
}

/**
 * The project's Architecture tab (LEGION-191 slice 1a): the component model as a tree of rows
 * with fill bars and `D/N issues done`, a chevron into a component's children with a breadcrumb
 * back, a component's details below the rows, and the Unassigned / Not architectural lists with
 * the component picker. Reads `GET /projects/{key}/architecture`; `api/event-refresh.ts` refreshes
 * it on an issue write that moves a status, a parent or a component attachment (or does not say
 * what it moved), on any other write of an issue the tree lists (a work row shows its title,
 * status, priority, links and `updated_at`; a side list its title and status), and on every
 * import event, so a bar moves in place when an issue closes.
 */
export function ArchitecturePage({ project }: { project: string }): ReactNode {
  const queryClient = useQueryClient();
  const tree = useQuery({
    queryKey: ["architecture", project],
    queryFn: () => api.getArchitecture(project),
    staleTime: 15_000,
  });
  // A rejected model comes back 200 with the reason in last_error; either way the tree's
  // source line settles from the refetch. A failure of the request itself (transport, 5xx) has
  // nowhere to land on the source row, so it renders beside that source's button until the
  // next attempt.
  const refresh = useMutation({
    mutationFn: (repo: string) => api.syncArchitectureSource(project, repo),
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: ["architecture", project] });
      // Its key prefixes the project's own list, so both refresh.
      void queryClient.invalidateQueries({ queryKey: architectureSourcesQuery().queryKey });
    },
  });
  const { component: selectedId, update, view } = usePaneState();

  if (tree.isPending) {
    return <LoadingSkeleton label="Loading architecture" />;
  }
  if (tree.isError) {
    return (
      <QueryError
        message="Could not load the architecture."
        onRetry={() => void tree.refetch()}
        retrying={tree.isFetching}
      />
    );
  }
  return (
    <ArchitectureTreeView
      onRefresh={(repo) => refresh.mutate(repo)}
      project={project}
      refresh={
        refresh.isIdle
          ? null
          : {
              error: refresh.error === null ? null : refresh.error.message,
              pending: refresh.isPending,
              repo: refresh.variables,
            }
      }
      selectedId={selectedId}
      tree={tree.data}
      update={update}
      view={view}
    />
  );
}

/** The latest Refresh: which source it asked for, whether it is still out, and the error of the
 *  request itself if it failed. */
interface RefreshState {
  readonly error: string | null;
  readonly pending: boolean;
  readonly repo: string;
}

function ArchitectureTreeView({
  onRefresh,
  project,
  refresh,
  selectedId,
  tree,
  update,
  view,
}: {
  onRefresh: (repo: string) => void;
  project: string;
  refresh: RefreshState | null;
  selectedId: string | null;
  tree: ArchitectureTree;
  update: UpdatePaneState;
  view: PaneView | null;
}): ReactNode {
  const { childCounts, index } = useMemo(() => {
    const counts = new Map<string, number>();
    for (const component of tree.components) {
      if (component.parent !== null) {
        counts.set(component.parent, (counts.get(component.parent) ?? 0) + 1);
      }
    }
    return { childCounts: counts, index: indexComponents(tree) };
  }, [tree]);
  const hasChildren = (id: string) => (childCounts.get(id) ?? 0) > 0;
  const selected = selectedId === null ? undefined : index.get(selectedId);
  // The level is the selected component when it has children, else its parent (the roots when
  // nothing is selected or the selection is a root leaf).
  const levelId =
    selected === undefined ? null : hasChildren(selected.id) ? selected.id : selected.parent;
  const level = levelId === null ? undefined : index.get(levelId);
  const rows =
    view === "no-work"
      ? tree.components.filter((component) => !component.external && component.total === 0)
      : childComponents(tree.components, levelId);
  const now = Date.now();
  const distrusted = distrustedSources(tree.sources, now);
  // The empty state belongs to a project none of whose sources has imported a model yet.
  const neverImported =
    tree.components.length === 0 && tree.sources.every((source) => source.last_commit === null);

  // Focus follows a keyboard level change once the new rows have rendered: the effect runs
  // after every render and acts only when a level change armed it.
  const pendingFocus = useRef<string | null>(null);
  useEffect(() => {
    const target = pendingFocus.current;
    if (target === null) {
      return;
    }
    pendingFocus.current = null;
    const node =
      target === ""
        ? document.querySelector<HTMLElement>(ROW_SELECTOR)
        : document.querySelector<HTMLElement>(`[data-component-row="${target}"]`);
    node?.focus();
  });

  const rove = (delta: 1 | -1) =>
    roveFocus(
      [...document.querySelectorAll<HTMLElement>(ROW_SELECTOR)],
      rowAround(document.activeElement),
      delta
    );
  const focusedRowId = () => rowAround(document.activeElement)?.dataset.componentRow;
  const descend = () => {
    const id = focusedRowId();
    if (id === undefined || !hasChildren(id)) {
      return;
    }
    pendingFocus.current = "";
    update({ component: id, view: null });
  };
  const ascend = () => {
    if (level === undefined) {
      return;
    }
    pendingFocus.current = level.id;
    update({ component: level.parent, view: null });
  };
  useKeymapScope("architecture");
  useKeymap("architecture", [
    // Movement is what the arrow keys are for; a palette row that moves the cursor helps nobody.
    {
      id: "next",
      keys: "j",
      label: "Next component",
      palette: false,
      run: () => rove(1),
    },
    {
      id: "previous",
      keys: "k",
      label: "Previous component",
      palette: false,
      run: () => rove(-1),
    },
    {
      id: "arrows",
      keys: ["ArrowDown", "ArrowUp"],
      label: "Next / previous component while one is focused",
      run: (event) => rove(event.key === "ArrowDown" ? 1 : -1),
      when: () => rowAround(document.activeElement) !== null,
    },
    {
      // Both keys do the same thing, so the palette offers it; a row presses Enter.
      id: "open",
      keys: ["Enter", "o"],
      label: "Open the focused component: its children, else its details",
      palette: true,
      run: () => {
        const id = focusedRowId();
        if (id === undefined) {
          return;
        }
        if (hasChildren(id)) {
          descend();
        } else {
          rowAround(document.activeElement)?.querySelector("a")?.click();
        }
      },
      when: () => document.activeElement?.matches(ROW_SELECTOR) === true,
    },
    {
      // Out of the palette: on a row with children `open` above already goes into them, and on
      // a leaf this row would do nothing.
      id: "descend",
      keys: ["l", "ArrowRight"],
      label: "Into the focused component's children",
      palette: false,
      run: descend,
      when: () => rowAround(document.activeElement) !== null,
    },
    {
      // Both keys do the same thing, and its `when` needs no focused row, so it is a row.
      id: "ascend",
      keys: ["h", "ArrowLeft"],
      label: "Up one level",
      palette: true,
      run: ascend,
      when: () => level !== undefined,
    },
    {
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back to the component row, then out",
      run: () => {
        const active = document.activeElement;
        const row = rowAround(active);
        if (row === active) {
          (active as HTMLElement).blur();
        } else {
          row?.focus();
        }
      },
      when: () => rowAround(document.activeElement) !== null,
    },
  ]);

  const path = levelId === null ? [] : componentPath(index, levelId);
  const rowSearch = (id: string) => `?component=${encodeURIComponent(id)}`;
  return (
    <div className="space-y-4" data-testid="architecture-pane">
      <header className={`space-y-1 text-sm ${textSecondaryOnCanvas}`}>
        {tree.sources.map((source) => (
          <SourceLine
            key={source.repo}
            now={now}
            onRefresh={() => onRefresh(source.repo)}
            refresh={refresh}
            source={source}
          />
        ))}
        <p className="flex flex-wrap items-center gap-x-2 gap-y-1" data-testid="architecture-scope">
          <span>
            {tree.totals.issues_done}/{tree.totals.issues_total} issues done
          </span>
          <span aria-hidden="true">·</span>
          <ScopeLink
            active={view === "no-work"}
            count={tree.totals.components_without_work}
            label={
              tree.totals.components_without_work === 1
                ? "component with no tracked work"
                : "components with no tracked work"
            }
            onToggle={() => update({ view: view === "no-work" ? null : "no-work" })}
          />
          <span aria-hidden="true">·</span>
          <ScopeLink
            active={view === "unassigned"}
            count={tree.totals.unassigned}
            label="unassigned"
            onToggle={() => update({ view: view === "unassigned" ? null : "unassigned" })}
          />
          <span aria-hidden="true">·</span>
          <ScopeLink
            active={view === "none"}
            count={tree.totals.not_architectural}
            label="not architectural"
            onToggle={() => update({ view: view === "none" ? null : "none" })}
          />
          {tree.totals.retired_links === 0 ? null : (
            <>
              <span aria-hidden="true">·</span>
              <button
                aria-pressed={view === "retired"}
                className={`inline-flex min-h-11 items-center rounded-full px-2 text-xs font-medium md:min-h-6 ${badgeHigh.bg} ${badgeHigh.text}`}
                onClick={() => update({ view: view === "retired" ? null : "retired" })}
                type="button"
              >
                {tree.totals.retired_links}{" "}
                {tree.totals.retired_links === 1 ? "issue points" : "issues point"} at retired
                components
              </button>
            </>
          )}
        </p>
      </header>

      <nav
        aria-label="Component level"
        className={`flex flex-wrap items-center gap-1 text-sm ${textSecondaryOnCanvas}`}
      >
        {view === "no-work" ? (
          <>
            <span className="font-medium">Components with no tracked work</span>
            <button
              className={`ml-2 min-h-11 text-xs font-medium md:min-h-0 ${linkText} ${linkHoverText}`}
              onClick={() => update({ view: null })}
              type="button"
            >
              Clear filter
            </button>
          </>
        ) : (
          <>
            {path.length === 0 ? (
              <span className="font-medium">All components</span>
            ) : (
              <Link
                className={`min-h-11 md:min-h-0 ${linkText} ${linkHoverText}`}
                to={{ search: "" }}
              >
                All components
              </Link>
            )}
            {path.map((crumb, position) => (
              <span className="flex items-center gap-1" key={crumb.id}>
                <span aria-hidden="true">/</span>
                {position === path.length - 1 ? (
                  <span className="font-medium">{crumb.title}</span>
                ) : (
                  <Link
                    className={`${linkText} ${linkHoverText}`}
                    to={{ search: rowSearch(crumb.id) }}
                  >
                    {crumb.title}
                  </Link>
                )}
              </span>
            ))}
          </>
        )}
      </nav>

      {selectedId !== null && selected === undefined ? (
        <p className={`text-sm ${inlineWarningText}`} role="status">
          Component {selectedId} is not in the current model
        </p>
      ) : null}

      {neverImported ? (
        <p className={textMutedOnCanvas} data-testid="architecture-empty">
          No model imported yet — Refresh reads .dispatch/architecture from{" "}
          {tree.sources.map((source) => `${source.repo}/${source.branch}`).join(", ")}.
        </p>
      ) : rows.length === 0 ? (
        <p className={textMutedOnCanvas}>
          {view === "no-work" ? "Every component has tracked work." : "No components here."}
        </p>
      ) : (
        <ul aria-label="Components" className="space-y-2">
          {rows.map((component) => (
            <ComponentRow
              component={component}
              components={tree.components}
              distrusted={distrusted.has(component.source)}
              hasChildren={hasChildren(component.id)}
              index={index}
              key={component.id}
              rowSearch={rowSearch}
              selected={component.id === selectedId}
              showPath={view === "no-work"}
            />
          ))}
        </ul>
      )}

      {view !== null && view !== "no-work" ? (
        <AttachmentLists project={project} tree={tree} view={view} />
      ) : selected !== undefined ? (
        <ComponentDetails
          component={selected}
          key={selected.id}
          project={project}
          rowSearch={rowSearch}
          tree={tree}
        />
      ) : null}
    </div>
  );
}

/** One source's header line: `repo/branch · at <sha8> · checked <time>` (or `never synced`) with
 *  its own Refresh, which imports that source alone; under it, when the source's last import
 *  failed, the error and the commit its components still come from. */
function SourceLine({
  now,
  onRefresh,
  refresh,
  source,
}: {
  now: number;
  onRefresh: () => void;
  refresh: RefreshState | null;
  source: ArchitectureTreeSource;
}): ReactNode {
  const state = sourceState(source, now);
  const mine = refresh !== null && refresh.repo === source.repo;
  const refreshing = mine && refresh.pending;
  return (
    <div data-testid="architecture-source">
      <p className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className={`font-medium ${textSecondaryOnCanvas}`}>
          {source.repo}/{source.branch}
        </span>
        {source.last_commit === null ? null : (
          <>
            <span aria-hidden="true">·</span>
            <span>
              at <code className="text-xs">{source.last_commit.slice(0, 8)}</code>
            </span>
          </>
        )}
        <span aria-hidden="true">·</span>
        {state === "never-synced" || source.last_sync_at === null ? (
          <span>never synced</span>
        ) : (
          <span className={state === "stale" ? inlineWarningText : undefined}>
            checked <Timestamp at={source.last_sync_at} />
            {state === "stale" ? " (stale)" : null}
          </span>
        )}
        <span aria-hidden="true">·</span>
        <button
          aria-label={refreshing ? `Refreshing ${source.repo}…` : `Refresh ${source.repo}`}
          className={`min-h-11 font-medium disabled:cursor-not-allowed disabled:opacity-50 md:min-h-0 ${linkText} ${linkHoverText}`}
          disabled={refresh?.pending === true}
          onClick={onRefresh}
          type="button"
        >
          {refreshing ? "Refreshing…" : "Refresh"}
        </button>
        {mine && refresh.error !== null ? (
          <span className={`text-xs ${dangerText}`} role="alert">
            {refresh.error}
          </span>
        ) : null}
      </p>
      {source.last_error === null ? null : (
        <div
          className={`mt-1 rounded-lg border p-3 text-sm ${calloutDangerBorder} ${calloutDangerBg} ${dangerText}`}
          role="alert"
        >
          Last import failed: {source.last_error}
          {source.last_commit === null
            ? null
            : ` · showing the model from ${source.last_commit.slice(0, 8)}`}
        </div>
      )}
    </div>
  );
}

function ScopeLink({
  active,
  count,
  label,
  onToggle,
}: {
  active: boolean;
  count: number;
  label: string;
  onToggle: () => void;
}): ReactNode {
  return (
    <button
      aria-pressed={active}
      className={`min-h-11 md:min-h-0 ${active ? "font-semibold" : ""} ${linkText} ${linkHoverText}`}
      onClick={onToggle}
      type="button"
    >
      {count} {label}
    </button>
  );
}

const TONE_LABEL: Record<ComponentTone, string> = {
  done: "all issues done",
  "done-with-gaps": "all issues done, contains components with no tracked work",
  external: "external",
  "no-work": "no tracked work",
  partial: "in progress",
  untouched: "not started",
};

function ComponentRow({
  component,
  components,
  distrusted,
  hasChildren,
  index,
  rowSearch,
  selected,
  showPath,
}: {
  component: ArchitectureTreeComponent;
  components: readonly ArchitectureTreeComponent[];
  distrusted: boolean;
  hasChildren: boolean;
  index: ComponentIndex;
  rowSearch: (id: string) => string;
  selected: boolean;
  showPath: boolean;
}): ReactNode {
  const tone = componentTone(component, components);
  const percent = component.total === 0 ? 0 : Math.round((component.done / component.total) * 100);
  const parents = showPath ? componentPath(index, component.id).slice(0, -1) : [];
  return (
    <li>
      <article
        aria-current={selected ? "true" : undefined}
        aria-label={`${component.title}, ${tone === "external" ? "external" : tone === "no-work" ? "no tracked work" : `${component.done}/${component.total} issues done`}`}
        className={`rounded-xl border p-3 ${card} ${cardHoverBorder} ${rowFocusRing} ${
          distrusted ? `border-dashed ${trustWarningBorder}` : selected ? borderStrong : ""
        }`}
        data-component-row={component.id}
        tabIndex={-1}
      >
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2 md:flex-nowrap">
          <div className="flex min-w-0 flex-1 items-center gap-2">
            {hasChildren ? (
              <Link
                aria-label={`Open the components inside ${component.title}`}
                className={`grid size-11 shrink-0 place-items-center rounded-lg md:size-8 ${textSecondaryOnSurface}`}
                to={{ search: rowSearch(component.id) }}
              >
                <svg aria-hidden="true" className="h-4 w-4" fill="none" viewBox="0 0 16 16">
                  <path
                    d="M6 4l4 4-4 4"
                    stroke="currentColor"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    strokeWidth="1.5"
                  />
                </svg>
              </Link>
            ) : (
              <span aria-hidden="true" className="size-11 shrink-0 md:size-8" />
            )}
            <div className="min-w-0 flex-1">
              {parents.length === 0 ? null : (
                <p className={`truncate text-xs ${textMutedOnSurface}`}>
                  {parents.map((parent) => parent.title).join(" / ")}
                </p>
              )}
              {/* Phone styles.css makes every `a` inline-flex: as an inline-level box a `block`
                  link shrink-wraps to its nowrap title and widens the page, and a nowrap title
                  straight inside an inline-flex link is an anonymous flex item that cannot
                  shrink. So the link is a flex item (its width comes from the flex algorithm)
                  and the title a `min-w-0` item that truncates inside it. */}
              <div className="flex min-w-0">
                <Link
                  className={`flex min-h-11 min-w-0 flex-1 items-center py-2 font-medium md:min-h-0 md:py-0 ${textPrimaryOnSurface} ${linkHoverText}`}
                  to={{ search: rowSearch(component.id) }}
                >
                  <TruncatedText className="min-w-0 flex-1" title={component.title}>
                    {component.title}
                  </TruncatedText>
                </Link>
              </div>
            </div>
            {tone === "external" ? (
              <span
                className={`shrink-0 rounded-full px-2 py-0.5 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
              >
                external
              </span>
            ) : null}
            {tone === "done-with-gaps" ? (
              <span
                className={`shrink-0 rounded-full px-2 py-0.5 text-xs font-medium ${badgeHigh.bg} ${badgeHigh.text}`}
              >
                contains components with no tracked work
              </span>
            ) : null}
          </div>
          {tone === "external" ? null : (
            <div className="flex w-full items-center gap-3 md:w-80 md:shrink-0">
              {tone === "no-work" ? (
                <div
                  aria-hidden="true"
                  className={`h-2 flex-1 rounded-full border border-dashed ${borderStrong}`}
                  data-tone={tone}
                />
              ) : (
                <div
                  aria-hidden="true"
                  className={`h-2 flex-1 overflow-hidden rounded-full ${tone === "done" ? progressFillDone : progressTrack}`}
                  data-tone={tone}
                >
                  {tone === "partial" || tone === "done-with-gaps" ? (
                    <div
                      className={`h-full rounded-full ${progressFill}`}
                      data-testid="progress-fill"
                      style={{ width: `${percent}%` }}
                    />
                  ) : null}
                </div>
              )}
              <span
                className={`w-32 shrink-0 text-right text-sm ${textSecondaryOnSurface}`}
                title={TONE_LABEL[tone]}
              >
                {tone === "no-work"
                  ? "No tracked work"
                  : `${component.done}/${component.total} issues done`}
              </span>
            </div>
          )}
        </div>
      </article>
    </li>
  );
}
