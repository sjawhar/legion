import { snippetSegments } from "@legion/contracts/dispatch-snippet";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { useQuery } from "@tanstack/react-query";
import {
  Fragment,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
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
import { buildProjectPath, type DispatchReferenceRoute } from "../refs/routes";
import { appKeymap, DIALOG_SCOPE, type KeymapAction, useKeymap } from "../shell/keymap";
import { KeyHints } from "../shell/ShortcutHelp";
import { useCloseOnNavigation, useDialog } from "../shell/useDialog";
import { groupResults, kindLabel, optionId, stepActive } from "./search-model";

const emptyResults: SearchResult[] = [];
const emptyProjects: Project[] = [];
const ACTIONS_GROUP_ID = "search-group-actions";
const MORE_ACTIONS_GROUP_ID = "search-group-more-actions";
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

/** One row of the listbox, a hit or a command: the selected outline and fill, and a click, Enter
 *  or Space selecting it. `layout` arranges the row's own content. */
function PaletteOption({
  active,
  children,
  id,
  layout = "",
  onSelect,
  reference,
}: {
  active: boolean;
  children: ReactNode;
  id: string;
  layout?: string;
  onSelect: () => void;
  /** A hit's target, which makes the row a `RefPreview` trigger. */
  reference?: DispatchReferenceRoute;
}): ReactNode {
  return (
    <div
      aria-selected={active}
      className={`relative isolate min-h-11 cursor-pointer border-b px-3 py-2 last:border-b-0 ${layout} ${borderDefault} ${
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
      {...(reference === undefined ? undefined : referenceTriggerProps(reference))}
    >
      {active ? (
        <div
          aria-hidden="true"
          className={`pointer-events-none absolute inset-0 -z-10 ${selectedCardBg}`}
        />
      ) : null}
      {children}
    </div>
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

  return (
    <PaletteOption
      active={active}
      id={optionId(result)}
      onSelect={onSelect}
      reference={referenceRouteFromHref(result.href)}
    >
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
    </PaletteOption>
  );
}

/** An action or project row: a label and an optional trailing hint. */
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
    <PaletteOption
      active={active}
      id={id}
      layout="flex items-center justify-between gap-4"
      onSelect={onSelect}
    >
      <span className={`min-w-0 truncate text-sm ${textPrimaryOnSurface}`}>{label}</span>
      {hint}
    </PaletteOption>
  );
}

/** One labelled run of command rows: the Actions a query leads to, the More actions it matches
 *  further in, or the Projects `g p` lists. */
interface CommandSection {
  id: string;
  label: string;
  options: readonly { hint: ReactNode; label: string; row: ActionRow | ProjectRow }[];
}

/* The heading is `aria-hidden` for the same reason as an owner row: the group below takes its
   name from it, and a reader would otherwise hear it twice. The group is a `fieldset` named by the
   heading, as each hit owner's group is, so a reader hears where the list changes from what this
   page can do to what the query found. */
function CommandGroup({
  activeId,
  onSelect,
  section,
}: {
  activeId: string | undefined;
  onSelect: (row: PaletteRow) => void;
  section: CommandSection;
}): ReactNode {
  if (section.options.length === 0) {
    return null;
  }
  return (
    <>
      <div
        aria-hidden="true"
        className={`flex min-w-0 items-center border-b px-3 py-1.5 text-xs font-semibold ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnSurfaceMuted}`}
        id={section.id}
        role="presentation"
      >
        {section.label}
      </div>
      <fieldset aria-labelledby={section.id} className="min-w-0">
        {section.options.map(({ hint, label, row }) => (
          <CommandOption
            active={row.id === activeId}
            hint={hint}
            id={row.id}
            key={row.id}
            label={label}
            onSelect={() => onSelect(row)}
          />
        ))}
      </fieldset>
    </>
  );
}

export function SearchPalette({
  mode,
  onClose,
}: {
  mode: PaletteMode | null;
  onClose: () => void;
}): ReactNode {
  // A chosen row runs once the palette is gone and `useDialog`'s cleanup has put focus back on
  // whatever opened it: run inside the palette, with focus in its input, it would act on nothing,
  // and a row that moves focus would have its move undone by that cleanup a moment later.
  const pendingAction = useRef<(() => void) | null>(null);
  // The last `/` search, which `/` reopens on: its hits do not depend on the page. `⌘K` and `g p`
  // open empty, since a query left there would filter a new page's actions, or its projects, down
  // to nothing.
  const lastSearch = useRef("");
  // On the commit that closes the palette React runs every passive-effect cleanup, the unmounted
  // palette's `useDialog` among them, before any passive-effect setup, so focus is back by the
  // time this effect runs the chosen row.
  useEffect(() => {
    if (mode !== null) {
      return;
    }
    const run = pendingAction.current;
    pendingAction.current = null;
    run?.();
  }, [mode]);

  if (mode === null) {
    return null;
  }
  // Mounted once per open, so each open starts on its first row with the rows of the page it
  // opened over, and a closed palette holds no search of its own.
  return (
    <PaletteDialog
      initialQuery={mode === "search" ? lastSearch.current : ""}
      key={mode}
      mode={mode}
      onAction={(run) => {
        pendingAction.current = run;
      }}
      onClose={onClose}
      onQuery={(query) => {
        if (mode === "search") {
          lastSearch.current = query;
        }
      }}
    />
  );
}

function PaletteDialog({
  initialQuery,
  mode,
  onAction,
  onClose,
  onQuery,
}: {
  initialQuery: string;
  mode: PaletteMode;
  onAction: (run: () => void) => void;
  onClose: () => void;
  onQuery: (query: string) => void;
}): ReactNode {
  const [query, setQuery] = useState(initialQuery);
  const [debouncedQuery, setDebouncedQuery] = useState(() => initialQuery.trim());
  const [activeId, setActiveId] = useState<string | null>(null);
  // A query the open starts with is selected when the input first takes focus, so typing
  // replaces it and Enter still opens its highlighted hit.
  const selectOnFocus = useRef(initialQuery !== "");
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();
  // The rows this open offers, decided on its first render, while focus is still on whatever
  // opened the palette, since `useDialog` moves it into the input after that commit. Read any
  // later and a `when()` that asks what is focused would answer "the palette", withdrawing every
  // row bound to the card, row or control the reader was actually on.
  const [actions] = useState(() => (mode === "all" ? appKeymap.actions() : []));
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: inputRef, onClose, open: true });
  // Every global key is masked while any dialog is open; the palette keeps `$mod+k` as its own
  // close, so pressing it twice still opens and closes.
  useKeymap(DIALOG_SCOPE, [
    {
      id: "search-close",
      inEditable: true,
      keys: "$mod+k",
      label: "Close search",
      run: onClose,
    },
  ]);
  const searchText = query.trim();
  const needle = searchText.toLowerCase();
  const searching = mode !== "projects";
  const tooShort = searchText.length < 2;
  // The server refuses a longer query; name the limit here instead of sending it.
  const tooLong = searchText.length > SEARCH_QUERY_MAX;
  const queryEnabled = searching && !tooShort && !tooLong;

  useEffect(() => {
    const timeout = window.setTimeout(() => setDebouncedQuery(searchText), 150);
    return () => window.clearTimeout(timeout);
  }, [searchText]);

  const search = useQuery({
    enabled: searching && debouncedQuery.length >= 2 && debouncedQuery.length <= SEARCH_QUERY_MAX,
    queryFn: () => api.search(debouncedQuery),
    queryKey: ["search", debouncedQuery],
  });
  const projects = useQuery({ ...projectsQuery(), enabled: mode === "projects" });
  const results = search.data?.results ?? emptyResults;
  const groups = groupResults(results);
  // An action whose label the query starts (every action, while the query is empty) is the one
  // the reader is typing, and it heads the list. One that holds the query further in (`issue` in
  // `Close issue`, `status` in `Move card to the next status`) is a coincidence of words: it is
  // listed below the hits and never highlighted for the reader, so a search and Enter opens a
  // hit, and such an action runs only once the reader has arrowed to it.
  const actionOptions = actions
    .filter((action) => action.label.toLowerCase().includes(needle))
    .map((action) => {
      const row: ActionRow = {
        action,
        id: `search-option-action-${action.scope}-${action.id}`,
        kind: "action",
      };
      return {
        hint: <KeyHints keys={action.keys} />,
        label: action.label,
        leads: action.label.toLowerCase().startsWith(needle),
        row,
      };
    });
  const projectRows: ProjectRow[] =
    mode === "projects"
      ? (projects.data ?? emptyProjects)
          .filter((project) => `${project.key} ${project.name}`.toLowerCase().includes(needle))
          .map((project) => ({
            id: `search-option-project-${project.key}`,
            kind: "project",
            project,
          }))
      : [];
  // The hits answer the query in the box, so a query still in flight shows none: the message
  // below the list says which of "type more", "searching", "failed" and "nothing" it is. A
  // refresh that fails keeps the answer the query already has, and so do the hits.
  const waitingForQuery = queryEnabled && debouncedQuery !== searchText;
  const showHits = queryEnabled && !waitingForQuery && search.data !== undefined;
  const hitGroups = showHits
    ? groups.map(({ owner, results: ownerResults }) => ({
        owner,
        rows: ownerResults.map(
          (result): ResultRow => ({ id: optionId(result), kind: "result", result })
        ),
      }))
    : [];
  const commandSections: readonly CommandSection[] = [
    {
      id: ACTIONS_GROUP_ID,
      label: "Actions",
      options: actionOptions.filter((option) => option.leads),
    },
    {
      id: PROJECTS_GROUP_ID,
      label: "Projects",
      options: projectRows.map((row) => ({
        hint: (
          <span className={`shrink-0 font-mono text-xs ${textMutedOnSurface}`}>
            {row.project.key}
          </span>
        ),
        label: row.project.name,
        row,
      })),
    },
  ];
  const moreActions: CommandSection = {
    id: MORE_ACTIONS_GROUP_ID,
    label: "More actions",
    options: actionOptions.filter((option) => !option.leads),
  };
  // What the list shows, in the order it shows it: the arrows walk this same list.
  const rows: PaletteRow[] = [
    ...commandSections.flatMap((section) => section.options.map((option) => option.row)),
    ...hitGroups.flatMap((group) => group.rows),
    ...moreActions.options.map((option) => option.row),
  ];
  // The highlight is the row the reader sees highlighted, held by its id: hits arriving below
  // the actions, or a refetch re-ranking them, leave it on that row. A new query sends it to the
  // head, and so does its row leaving the list; the head it lands on is then adopted, so the row
  // coming back does not take the highlight back from it. The head is the first row unless that
  // is one of the More actions, which come last: with only those listed, nothing is highlighted,
  // and Enter runs nothing until the reader arrows.
  const chosen = rows.findIndex((row) => row.id === activeId);
  const activeIndex = chosen === -1 && rows.length > moreActions.options.length ? 0 : chosen;
  const activeRow = rows[activeIndex];
  const shownId = activeRow?.id ?? null;
  if (shownId !== activeId) {
    setActiveId(shownId);
  }
  // Focus stays in the input while the arrows move the highlight (`aria-activedescendant`), so
  // the list scrolls the highlighted row into its own view: an issue page offers more rows than
  // the list's height holds. It does again whenever the list's own height changes (the window
  // shrinks, or the message under it grows), since the row did not move but its view did.
  useLayoutEffect(() => {
    if (shownId === null) {
      return;
    }
    const keepInView = () => {
      document.getElementById(shownId)?.scrollIntoView({ block: "nearest" });
    };
    keepInView();
    const list = listRef.current;
    if (list === null) {
      return;
    }
    const sizes = new ResizeObserver(keepInView);
    sizes.observe(list);
    return () => sizes.disconnect();
  }, [shownId]);

  useCloseOnNavigation(true, onClose);

  const selectRow = (row: PaletteRow) => {
    if (row.kind === "action") {
      onAction(row.action.run);
    } else {
      navigate(
        row.kind === "project"
          ? buildProjectPath({ kind: "project", project: row.project.key })
          : row.result.href
      );
    }
    onClose();
  };
  // While the query's search is out - its first answer, or a refetch behind a cached one - its
  // hits can still arrive above the More actions, so an arrow aimed at the list now would land
  // on whatever row then sits there: until the search answers, the arrows walk only the rows
  // above the More actions, and with none they do nothing. A highlight the reader moved into the
  // More actions after an earlier answer keeps moving over the whole list. `isFetching`, not the
  // message's `isPending`: with a stale cached answer `isPending` is false while the refetch runs.
  const searchOut = waitingForQuery || (queryEnabled && search.isFetching);
  const firstMore = rows.length - moreActions.options.length;
  const walkable = searchOut && activeIndex < firstMore ? firstMore : rows.length;
  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if ((event.key === "ArrowDown" || event.key === "ArrowUp") && walkable > 0) {
      event.preventDefault();
      const delta = event.key === "ArrowDown" ? 1 : -1;
      // From no highlight, Down takes the first row and Up the last.
      const next =
        activeIndex === -1
          ? delta === 1
            ? 0
            : walkable - 1
          : stepActive(activeIndex, delta, walkable);
      setActiveId(rows[next]?.id ?? null);
    } else if (event.key === "Enter" && activeRow !== undefined) {
      event.preventDefault();
      selectRow(activeRow);
    }
  };

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      {/* The wrapper holds the palette's place: below the top bar, to the screen's foot (1rem
          short of it from `xl`). The dialog is a column no taller than that, in which only the
          list scrolls, and the list keeps one row's height, so a short window cuts the key hints
          and then the message rather than the row the reader is on. */}
      <div className="pointer-events-none fixed inset-x-0 top-12 bottom-0 z-50 flex items-start justify-center xl:top-20 xl:bottom-4 xl:px-4">
        <div
          aria-label="Search"
          aria-modal="true"
          className={`pointer-events-auto flex max-h-full w-full flex-col overflow-hidden rounded-b-lg border shadow-2xl xl:max-w-2xl xl:rounded-lg ${card} ${borderDefault}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className={`shrink-0 border-b p-3 ${borderDefault}`}>
            <input
              aria-activedescendant={activeRow?.id}
              aria-controls="search-results"
              aria-expanded={rows.length > 0}
              aria-label="Search"
              className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
              onChange={(event) => {
                setQuery(event.target.value);
                onQuery(event.target.value);
                // A new query re-ranks the list, so the highlight returns to its head.
                setActiveId(null);
              }}
              onFocus={(event) => {
                if (selectOnFocus.current) {
                  selectOnFocus.current = false;
                  event.currentTarget.select();
                }
              }}
              onKeyDown={handleKeyDown}
              placeholder={mode === "projects" ? "Go to project" : "Search Dispatch"}
              ref={inputRef}
              role="combobox"
              type="search"
              value={query}
            />
          </div>
          {rows.length === 0 ? null : (
            <div
              className="max-h-[60vh] min-h-20 flex-1 overflow-y-auto"
              id="search-results"
              ref={listRef}
              role="listbox"
            >
              {commandSections.map((section) => (
                <CommandGroup
                  activeId={activeRow?.id}
                  key={section.id}
                  onSelect={selectRow}
                  section={section}
                />
              ))}
              {hitGroups.map(({ owner, rows: hitRows }) => {
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
                      {hitRows.map((row) => (
                        <ResultOption
                          active={activeRow?.id === row.id}
                          muted={muted}
                          key={row.id}
                          onSelect={() => selectRow(row)}
                          result={row.result}
                        />
                      ))}
                    </fieldset>
                  </Fragment>
                );
              })}
              <CommandGroup activeId={activeRow?.id} onSelect={selectRow} section={moreActions} />
            </div>
          )}
          {mode === "projects" ? (
            projects.isPending ? (
              <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>
                Loading projects…
              </p>
            ) : projects.isError ? (
              <div className="shrink-0 p-3">
                <QueryError
                  message="Could not load projects."
                  onRetry={() => {
                    void projects.refetch();
                  }}
                  retrying={projects.isFetching}
                />
              </div>
            ) : projectRows.length === 0 ? (
              <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>
                {searchText === "" ? "No projects" : `No projects match "${searchText}"`}
              </p>
            ) : null
          ) : tooShort ? (
            <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>
              Type at least 2 characters
            </p>
          ) : tooLong ? (
            <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>
              Search with a short phrase: {searchText.length} characters is over the{" "}
              {SEARCH_QUERY_MAX}-character limit
            </p>
          ) : waitingForQuery || search.isPending ? (
            <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>Searching…</p>
          ) : search.isError && search.data === undefined ? (
            <div className="shrink-0 p-3">
              <QueryError
                message="Search failed."
                onRetry={() => {
                  void search.refetch();
                }}
                retrying={search.isFetching}
              />
            </div>
          ) : results.length === 0 ? (
            <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>
              No results for &quot;{searchText}&quot;
            </p>
          ) : null}
          {/* The palette is a keyboard surface and never said so: the keys that drive it sit
              at its foot, quieter than any hit above them - and only where there is a pointer
              that can hover, since a touch reader has none of these keys and the row would
              cost them 33px of hits. */}
          <div
            className={`hidden shrink-0 flex-wrap items-center gap-x-3 gap-y-1 border-t px-3 py-2 text-xs pointer-fine:flex ${borderDefault} ${textMutedOnSurface}`}
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
