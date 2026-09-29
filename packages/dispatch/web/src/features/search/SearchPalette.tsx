import { snippetSegments } from "@legion/contracts/dispatch-snippet";
import { useQuery } from "@tanstack/react-query";
import { Fragment, type KeyboardEvent, type ReactNode, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { projectsQuery } from "../../api/queries";
import type { Project, SearchResult, SearchResultKind } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  backdrop50,
  badgeLow,
  borderDefault,
  card,
  inputClasses,
  kbdHint,
  searchHitBg,
  searchHitText,
  selectedCardBg,
  selectedCardBorder,
  surfaceMutedBg,
  textMutedOnSelectedCard,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
  textSecondaryOnSurfaceMuted,
} from "../../theme/classes";
import { statusText } from "../project/board-model";
import { referenceRouteFromHref } from "../refs/RefLink";
import { referenceTriggerProps } from "../refs/RefPreview";
import { appKeymap, DIALOG_SCOPE, type KeymapAction, useKeymap } from "../shell/keymap";
import { KeyHints } from "../shell/ShortcutHelp";
import { useCloseOnNavigation, useDialog } from "../shell/useDialog";
import { groupResults, kindLabel, optionId, stepActive } from "./search-model";

const emptyResults: SearchResult[] = [];
const emptyActions: KeymapAction[] = [];
const emptyProjects: Project[] = [];
const ACTIONS_GROUP_ID = "search-group-actions";
const PROJECTS_GROUP_ID = "search-group-projects";

/** Which list the palette is: this page's actions and search hits, search alone, or projects. */
export type PaletteMode = "all" | "search" | "projects";

interface ActionRow {
  action: KeymapAction;
  id: string;
  kind: "action";
}

interface ProjectRow {
  id: string;
  kind: "project";
  project: Project;
}

interface ResultRow {
  id: string;
  kind: "result";
  result: SearchResult;
}

/** One selectable line, whatever it came from: arrows, Enter and `aria-activedescendant` see only these. */
type PaletteRow = ActionRow | ProjectRow | ResultRow;

function SearchResultIcon({ kind }: { kind: SearchResultKind }): ReactNode {
  const common = {
    "aria-hidden": true,
    className: "size-4 shrink-0",
    fill: "none",
    stroke: "currentColor",
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    strokeWidth: 1.75,
    viewBox: "0 0 24 24",
  };

  if (kind === "issue") {
    return (
      <svg {...common}>
        <title>Issue result</title>
        <circle cx="12" cy="12" r="8" />
        <path d="M12 8v4l2.5 2" />
      </svg>
    );
  }
  if (kind === "document") {
    return (
      <svg {...common}>
        <title>Document result</title>
        <path d="M7 3h7l3 3v15H7z" />
        <path d="M14 3v4h4M10 12h4M10 16h4" />
      </svg>
    );
  }
  if (kind === "comment") {
    return (
      <svg {...common}>
        <title>Comment result</title>
        <path d="M5 6.5h14v10H9l-4 3z" />
        <path d="M9 10h6M9 13h4" />
      </svg>
    );
  }
  if (kind === "ask") {
    return (
      <svg {...common}>
        <title>Ask result</title>
        <circle cx="12" cy="12" r="8" />
        <path d="M9.8 9.5a2.3 2.3 0 1 1 3.3 2.1c-.9.5-1.1.9-1.1 1.9M12 16h.01" />
      </svg>
    );
  }
  return (
    <svg {...common}>
      <title>Message result</title>
      <path d="M5 6.5h14v10H9l-4 3z" />
      <path d="M9 10h6M9 13h6" />
    </svg>
  );
}

function ResultOption({
  active,
  muted,
  onSelect,
  result,
}: {
  active: boolean;
  muted: boolean;
  onSelect: () => void;
  result: SearchResult;
}): ReactNode {
  const segments = snippetSegments(result.snippet);
  let segmentStart = 0;
  const route = referenceRouteFromHref(result.href);

  return (
    <div
      aria-selected={active}
      className={`relative isolate min-h-11 cursor-pointer border-b px-3 py-2 last:border-b-0 ${borderDefault} ${
        active ? selectedCardBorder : ""
      }`}
      id={optionId(result)}
      onClick={onSelect}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onSelect();
        }
      }}
      role="option"
      tabIndex={-1}
      {...(route === undefined ? undefined : referenceTriggerProps(route))}
    >
      {active ? (
        <div
          aria-hidden="true"
          className={`pointer-events-none absolute inset-0 -z-10 ${selectedCardBg}`}
        />
      ) : null}
      <div
        className={`flex min-w-0 items-center gap-2 text-xs ${
          muted ? textMutedOnSelectedCard : textSecondaryOnSurface
        }`}
      >
        <SearchResultIcon kind={result.kind} />
        <span className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}>
          {kindLabel(result.kind)}
        </span>
        {result.artifact === undefined ? null : (
          <span className="truncate">{result.artifact.name}</span>
        )}
      </div>
      <p
        className={`mt-1 line-clamp-2 text-sm ${
          muted ? textMutedOnSelectedCard : textPrimaryOnSurface
        }`}
      >
        {segments.map((segment) => {
          const key = `${segmentStart}-${segment.mark}`;
          segmentStart += segment.text.length;
          return segment.mark ? (
            <mark className={`rounded px-0.5 ${searchHitBg} ${searchHitText}`} key={key}>
              {segment.text}
            </mark>
          ) : (
            <Fragment key={key}>{segment.text}</Fragment>
          );
        })}
      </p>
    </div>
  );
}

/** An action or project row: a label, an optional trailing hint, and the hits' selection contract. */
function CommandOption({
  active,
  hint,
  id,
  label,
  onSelect,
}: {
  active: boolean;
  hint: ReactNode;
  id: string;
  label: string;
  onSelect: () => void;
}): ReactNode {
  return (
    <div
      aria-selected={active}
      className={`relative isolate flex min-h-11 cursor-pointer items-center justify-between gap-4 border-b px-3 py-2 last:border-b-0 ${borderDefault} ${
        active ? selectedCardBorder : ""
      }`}
      id={id}
      onClick={onSelect}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onSelect();
        }
      }}
      role="option"
      tabIndex={-1}
    >
      {active ? (
        <div
          aria-hidden="true"
          className={`pointer-events-none absolute inset-0 -z-10 ${selectedCardBg}`}
        />
      ) : null}
      <span className={`min-w-0 truncate text-sm ${textPrimaryOnSurface}`}>{label}</span>
      {hint}
    </div>
  );
}

/* `aria-hidden` for the same reason as an owner row: the group below takes its name from this
   heading, and a reader would otherwise hear it twice. */
function GroupHeading({ id, label }: { id: string; label: string }): ReactNode {
  return (
    <div
      aria-hidden="true"
      className={`flex min-w-0 items-center border-b px-3 py-1.5 text-xs font-semibold ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnSurfaceMuted}`}
      id={id}
      role="presentation"
    >
      {label}
    </div>
  );
}

export function SearchPalette({
  mode,
  onClose,
}: {
  mode: PaletteMode | null;
  onClose: () => void;
}): ReactNode {
  const open = mode !== null;
  const [query, setQuery] = useState("");
  const [debouncedQuery, setDebouncedQuery] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const navigate = useNavigate();
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: inputRef, onClose, open });
  // The global `$mod+k` toggle is masked while any dialog is open; the palette keeps the key as
  // its own close so pressing it twice still opens and closes.
  useKeymap(
    DIALOG_SCOPE,
    open
      ? [
          {
            id: "search-close",
            inEditable: true,
            keys: "$mod+k",
            label: "Close search",
            run: onClose,
          },
        ]
      : []
  );
  const searchText = query.trim();
  const needle = searchText.toLowerCase();
  const searching = mode !== "projects";
  const queryEnabled = searching && searchText.length >= 2;

  // Each open starts empty. The palette stays mounted while closed, so a query left behind would
  // come back on the next open and filter the new page's actions down to nothing.
  useEffect(() => {
    if (mode !== null) {
      setQuery("");
      setDebouncedQuery("");
    }
  }, [mode]);

  useEffect(() => {
    const timeout = window.setTimeout(() => setDebouncedQuery(searchText), 150);
    return () => window.clearTimeout(timeout);
  }, [searchText]);

  const search = useQuery({
    enabled: searching && debouncedQuery.length >= 2,
    queryFn: () => api.search(debouncedQuery),
    queryKey: ["search", debouncedQuery],
  });
  const projects = useQuery({ ...projectsQuery(), enabled: mode === "projects" });
  const results = search.data?.results ?? emptyResults;
  const groups = groupResults(results);
  const visibleResults = groups.flatMap(({ results }) => results);
  // Read live rather than snapshotted, so the rail's Search control and `$mod+k` agree and a
  // `when()` that changed while the palette was open is honoured.
  const actions = mode === "all" ? appKeymap.actions() : emptyActions;
  const actionRows: ActionRow[] = actions
    .filter((action) => needle === "" || action.label.toLowerCase().includes(needle))
    .map((action) => ({
      action,
      id: `search-option-action-${action.scope}-${action.id}`,
      kind: "action",
    }));
  const projectRows: ProjectRow[] =
    mode === "projects"
      ? (projects.data ?? emptyProjects)
          .filter(
            (project) =>
              needle === "" || `${project.key} ${project.name}`.toLowerCase().includes(needle)
          )
          .map((project) => ({
            id: `search-option-project-${project.key}`,
            kind: "project",
            project,
          }))
      : [];
  // The hits answer the query in the box, so a query still in flight shows none: the message
  // below the list says which of "type more", "searching", "failed" and "nothing" it is.
  const waitingForQuery = queryEnabled && debouncedQuery !== searchText;
  const showHits = queryEnabled && !waitingForQuery && !search.isPending && !search.isError;
  const resultRows: ResultRow[] = showHits
    ? visibleResults.map((result) => ({ id: optionId(result), kind: "result", result }))
    : [];
  const rows: PaletteRow[] = [...actionRows, ...projectRows, ...resultRows];

  // Anything that re-ranks the list — a new query, another mode, hits arriving — returns the
  // highlight to its head rather than to whatever row now sits at the old index.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the dependencies are the re-ranking events, not values the effect reads
  useEffect(() => setActiveIndex(0), [searchText, mode, search.dataUpdatedAt]);

  useCloseOnNavigation(open, onClose);

  if (!open) {
    return null;
  }

  const activeRow = rows[activeIndex];
  const selectRow = (row: PaletteRow) => {
    if (row.kind === "action") {
      row.action.run();
    } else {
      navigate(row.kind === "project" ? `/projects/${row.project.key}` : row.result.href);
    }
    onClose();
  };
  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown" && rows.length > 0) {
      event.preventDefault();
      setActiveIndex((index) => stepActive(index, 1, rows.length));
    } else if (event.key === "ArrowUp" && rows.length > 0) {
      event.preventDefault();
      setActiveIndex((index) => stepActive(index, -1, rows.length));
    } else if (event.key === "Enter" && activeRow !== undefined) {
      event.preventDefault();
      selectRow(activeRow);
    }
  };

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-x-0 top-12 z-50 flex justify-center xl:top-20 xl:px-4">
        <div
          aria-label="Search"
          aria-modal="true"
          className={`pointer-events-auto w-full overflow-hidden rounded-b-lg border shadow-2xl xl:max-w-2xl xl:rounded-lg ${card} ${borderDefault}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className={`border-b p-3 ${borderDefault}`}>
            <input
              aria-activedescendant={activeRow?.id}
              aria-controls="search-results"
              aria-expanded={rows.length > 0}
              aria-label="Search"
              className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
              onChange={(event) => setQuery(event.target.value)}
              onKeyDown={handleKeyDown}
              placeholder={mode === "projects" ? "Go to project" : "Search Dispatch"}
              ref={inputRef}
              role="combobox"
              type="search"
              value={query}
            />
          </div>
          {rows.length === 0 ? null : (
            <div id="search-results" role="listbox">
              {actionRows.length === 0 ? null : (
                <>
                  <GroupHeading id={ACTIONS_GROUP_ID} label="Actions" />
                  {/* The hits' owner groups are fieldsets named by the row above them; the
                      actions are one more such group, so a reader hears where the list changes
                      from what this page can do to what the query found. */}
                  <fieldset aria-labelledby={ACTIONS_GROUP_ID} className="min-w-0">
                    {actionRows.map((row) => (
                      <CommandOption
                        active={activeRow?.id === row.id}
                        hint={<KeyHints keys={row.action.keys} />}
                        id={row.id}
                        key={row.id}
                        label={row.action.label}
                        onSelect={() => selectRow(row)}
                      />
                    ))}
                  </fieldset>
                </>
              )}
              {mode === "projects" ? (
                projectRows.length === 0 ? null : (
                  <>
                    <GroupHeading id={PROJECTS_GROUP_ID} label="Projects" />
                    <fieldset aria-labelledby={PROJECTS_GROUP_ID} className="min-w-0">
                      {projectRows.map((row) => (
                        <CommandOption
                          active={activeRow?.id === row.id}
                          hint={
                            <span className={`shrink-0 font-mono text-xs ${textMutedOnSurface}`}>
                              {row.project.key}
                            </span>
                          }
                          id={row.id}
                          key={row.id}
                          label={row.project.name}
                          onSelect={() => selectRow(row)}
                        />
                      ))}
                    </fieldset>
                  </>
                )
              ) : !showHits ? null : (
                groups.map(({ owner, results: ownerResults }) => {
                  const header =
                    owner.kind === "issue"
                      ? {
                          id: `issue:${owner.key}`,
                          key: owner.key,
                          name: owner.title,
                          status: owner.status,
                        }
                      : {
                          id: `document:${owner.artifact_id}`,
                          key: owner.project,
                          name: owner.name,
                          status: undefined,
                        };
                  const muted = header.status === "done";
                  return (
                    <Fragment key={header.id}>
                      {/* The group's own row: a label for the hits under it, on the recessed
                          surface so the hits read as the list and this reads as its heading.
                          Its status is the lifecycle label the rest of the product shows -
                          `Needs review`, never the raw value. */}
                      {/* `aria-hidden` because the group below takes its name from this row: a
                          reader would otherwise hear the owner twice, once as the row and once
                          as the group's label. `aria-labelledby` computes a name from a hidden
                          element, so the group keeps it. */}
                      <div
                        aria-hidden="true"
                        aria-label={`${header.key}: ${header.name}`}
                        className={`flex min-w-0 items-center gap-2 border-b px-3 py-1.5 text-xs ${borderDefault} ${surfaceMutedBg} ${
                          muted ? textMutedOnSurfaceMuted : ""
                        }`}
                        data-status={header.status}
                        id={`search-group-${header.id}`}
                        role="presentation"
                      >
                        <span className="font-semibold">{header.key}</span>
                        <span
                          className={`min-w-0 flex-1 truncate ${
                            muted ? textMutedOnSurfaceMuted : textSecondaryOnSurfaceMuted
                          }`}
                        >
                          {header.name}
                        </span>
                        {header.status === undefined ? null : (
                          <span
                            className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}
                          >
                            {statusText(header.status)}
                          </span>
                        )}
                      </div>
                      {/* The hits of one owner are a group named by the row above them, so a
                          reader who cannot see that row still hears which issue or document a
                          hit belongs to. The options stay direct children of the group and the
                          listbox's own arrow navigation is untouched. A `fieldset` carries the
                          group role implicitly; `min-w-0` overrides its UA `min-inline-size:
                          min-content`, which a long hit would otherwise widen the palette to. */}
                      <fieldset aria-labelledby={`search-group-${header.id}`} className="min-w-0">
                        {ownerResults.map((result) => {
                          const row: ResultRow = { id: optionId(result), kind: "result", result };
                          return (
                            <ResultOption
                              active={activeRow?.id === row.id}
                              muted={muted}
                              key={row.id}
                              onSelect={() => selectRow(row)}
                              result={result}
                            />
                          );
                        })}
                      </fieldset>
                    </Fragment>
                  );
                })
              )}
            </div>
          )}
          {mode === "projects" ? (
            projects.isPending ? (
              <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`}>Loading projects…</p>
            ) : projects.isError ? (
              <div className="p-3">
                <QueryError
                  message="Could not load projects."
                  onRetry={() => {
                    void projects.refetch();
                  }}
                  retrying={projects.isFetching}
                />
              </div>
            ) : projectRows.length === 0 ? (
              <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`}>
                {searchText === "" ? "No projects" : `No projects match "${searchText}"`}
              </p>
            ) : null
          ) : !queryEnabled ? (
            <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`}>Type at least 2 characters</p>
          ) : waitingForQuery || search.isPending ? (
            <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`}>Searching…</p>
          ) : search.isError ? (
            <div className="p-3">
              <QueryError
                message="Search failed."
                onRetry={() => {
                  void search.refetch();
                }}
                retrying={search.isFetching}
              />
            </div>
          ) : results.length === 0 ? (
            <p className={`px-3 py-3 text-sm ${textMutedOnSurface}`}>
              No results for &quot;{searchText}&quot;
            </p>
          ) : null}
          {/* The palette is a keyboard surface and never said so: the keys that drive it sit
              at its foot, quieter than any hit above them - and only where there is a pointer
              that can hover, since a touch reader has none of these keys and the row would
              cost them 33px of hits. */}
          <div
            className={`hidden flex-wrap items-center gap-x-3 gap-y-1 border-t px-3 py-2 text-xs pointer-fine:flex ${borderDefault} ${textMutedOnSurface}`}
            data-testid="search-keyboard-hints"
          >
            <span className="inline-flex items-center gap-1">
              <kbd className={`rounded px-1 py-0.5 font-medium ${kbdHint}`}>↑</kbd>
              <kbd className={`rounded px-1 py-0.5 font-medium ${kbdHint}`}>↓</kbd>
              to move
            </span>
            <span className="inline-flex items-center gap-1">
              <kbd className={`rounded px-1 py-0.5 font-medium ${kbdHint}`}>Enter</kbd>
              to open
            </span>
            <span className="inline-flex items-center gap-1">
              <kbd className={`rounded px-1 py-0.5 font-medium ${kbdHint}`}>Esc</kbd>
              to close
            </span>
          </div>
        </div>
      </div>
    </>
  );
}
