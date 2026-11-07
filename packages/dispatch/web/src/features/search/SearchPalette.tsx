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
import { useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { projectsQuery, whoAmIQuery } from "../../api/queries";
import type { Project, SearchOperator, SearchResult, SearchResultKind } from "../../api/types";
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
import { useProjectArtifact } from "../document/useProjectArtifact";
import { statusText } from "../project/board-model";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { searchSnippetMarkdown } from "../refs/markdown-fragment";
import { referenceTriggerProps } from "../refs/RefPreview";
import {
  buildIssuePath,
  buildProjectPath,
  type DispatchReferenceRoute,
  nodeRouteFromHref,
  routeProjectOf,
} from "../refs/routes";
import { appKeymap, DIALOG_SCOPE, type KeymapAction, useKeymap } from "../shell/keymap";
import { KeyHints } from "../shell/ShortcutHelp";
import { useCloseOnNavigation, useDialog } from "../shell/useDialog";
import { AskHitStatus } from "./AskHitStatus";
import {
  documentRouteOf,
  issueOfPage,
  readSelectedIssues,
  scopeAtCursor,
  scopeOfPage,
  scopeOfProject,
} from "./bar-scope";
import { NEW_ISSUE_GROUP_ID, NEW_ISSUE_OPTION_ID, useNewIssueOffer } from "./NewIssueOffer";
import { barNoteClassName } from "./offer";
import { type RecentItem, readRecents } from "./recents";
import { SearchFilterBar } from "./SearchFilterBar";
import { SearchOperators } from "./SearchOperators";
import {
  SEND_TO_AGENT_GROUP_ID,
  SEND_TO_AGENT_OPTION_ID,
  useSendToAgentOffer,
} from "./SendToAgentOffer";
import { type SearchBarFilters, searchRequest } from "./search-filters";
import { groupResults, kindLabel, optionId, stepActive } from "./search-model";

const emptyResults: SearchResult[] = [];
const emptyProjects: Project[] = [];
const emptyOperators: SearchOperator[] = [];
const ACTIONS_GROUP_ID = "search-group-actions";
const MORE_ACTIONS_GROUP_ID = "search-group-more-actions";
const PROJECTS_GROUP_ID = "search-group-projects";
const RECENT_GROUP_ID = "search-group-recent";

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

/** An item the person opened lately, which the bar lists back while its box is empty. */
interface RecentRow {
  id: string;
  item: RecentItem;
  kind: "recent";
}

/** The offer of the words in the box as a new issue, which the bar lists when its search
 *  matched nothing. */
interface NewIssueRow {
  id: string;
  kind: "new-issue";
}

/** The offer of the words in the box as a message to the agent on this issue, which the bar
 *  lists while a live session holds that issue's claim. */
interface SendToAgentRow {
  id: string;
  kind: "send-to-agent";
}

/** One selectable line, whatever it came from: arrows, Enter and `aria-activedescendant` see only these. */
type PaletteRow = ActionRow | NewIssueRow | ProjectRow | RecentRow | ResultRow | SendToAgentRow;

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
  // The server marks the matched words in the snippet's source text. The snippet renders as
  // Markdown (the text someone wrote, with its bold and code and references), which cannot
  // carry those marks through the parse, so the preview puts them back on the rendered text.
  // `ts_headline` returns a body whole when it fits its 24-word window, and cuts a longer one
  // to that window around the match; a cut can open a mark at either end, and the delimiter it
  // left goes before the parse (`balanceCutMarkdown`, which leaves a run CommonMark reads as
  // literal alone). The hits are matched by their words, so the display text can change.
  const segments = snippetSegments(result.snippet);
  const source = segments.map((segment) => segment.text).join("");
  const snippet = searchSnippetMarkdown(source);
  const hits = segments.flatMap((segment) => (segment.mark ? [segment.text] : []));

  return (
    <PaletteOption
      active={active}
      id={optionId(result)}
      onSelect={onSelect}
      reference={nodeRouteFromHref(result.href)}
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
      <MarkdownPreview
        className={`mt-1 text-sm ${muted ? textMutedOnSelectedCard : textPrimaryOnSurface}`}
        highlight={hits}
        highlightClassName={`rounded px-0.5 ${searchHitBg} ${searchHitText}`}
        lines={2}
        markdown={snippet}
      />
      {result.ask === undefined ? null : <AskHitStatus ask={result.ask} muted={muted} />}
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

/** One labelled run of command rows: the Recent items the bar opens on, the Actions a query
 *  leads to, the More actions it matches further in, the Projects it names, the New issue it
 *  offers for words that matched nothing, or the send to this issue's own agent. */
interface CommandSection {
  id: string;
  label: string;
  options: readonly {
    hint: ReactNode;
    label: string;
    row: ActionRow | NewIssueRow | ProjectRow | RecentRow | SendToAgentRow;
  }[];
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
  onClose,
  open,
}: {
  onClose: () => void;
  open: boolean;
}): ReactNode {
  // A chosen row runs once the bar is gone and `useDialog`'s cleanup has put focus back on
  // whatever opened it: run inside the bar, with focus in its input, it would act on nothing,
  // and a row that moves focus would have its move undone by that cleanup a moment later.
  const pendingAction = useRef<(() => void) | null>(null);
  // On the commit that closes the bar React runs every passive-effect cleanup, the unmounted
  // bar's `useDialog` among them, before any passive-effect setup, so focus is back by the
  // time this effect runs the chosen row.
  useEffect(() => {
    if (open) {
      return;
    }
    const run = pendingAction.current;
    pendingAction.current = null;
    run?.();
  }, [open]);

  if (!open) {
    return null;
  }
  // Mounted once per open, so each open starts on its first row with the rows, the chip and the
  // writing cursor of the page it opened over, and a closed bar holds no search of its own.
  return (
    <PaletteDialog
      onAction={(run) => {
        pendingAction.current = run;
      }}
      onClose={onClose}
    />
  );
}

function PaletteDialog({
  onAction,
  onClose,
}: {
  onAction: (run: () => void) => void;
  onClose: () => void;
}): ReactNode {
  const [query, setQuery] = useState("");
  const [debouncedQuery, setDebouncedQuery] = useState("");
  const [activeId, setActiveId] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();
  const location = useLocation();
  // The project of the page this open was made from (its own page, or an issue of it), decided
  // on the first render as the action rows are, so a hit the reader opens cannot change it
  // before the bar closes. It narrows nothing: a search from a project page searches
  // everything, as a search from anywhere does. The route reads it to tell which issue a query
  // that is a bare number names, and a reader who wants this project alone adds its chip.
  const [pageProject] = useState(() => routeProjectOf(location.pathname));
  const [filters, setFilters] = useState<SearchBarFilters>({});
  // What the bar will act on. The page seeds it, `Tab` narrows it to the block the writing
  // cursor was in and `Backspace` widens it to the project: all three are read on this first
  // render, the cursor because `useDialog` takes the focus a frame later and the document's
  // selection is no longer the page's once it has.
  const [blockScope] = useState(scopeAtCursor);
  const [projectScope] = useState(() => scopeOfProject(location.pathname));
  const [scopeLevel, setScopeLevel] = useState<"page" | "block" | "project">("page");
  // The issues the issue list had selected as this open was made, so no render of the list
  // behind the bar moves a chip the reader is looking at.
  const [selectedIssues] = useState(readSelectedIssues);
  // The document page's own read, under the key that page uses, so the chip names the document
  // by its name with no request of its own.
  const [documentRoute] = useState(() => documentRouteOf(location.pathname, location.search));
  // The issue the bar was opened over, decided on the first render as the rest of the page's
  // own values are, so a hit the reader arrows past cannot move what an offer would act on.
  const [pageIssue] = useState(() => issueOfPage(location.pathname, location.search));
  const documentName = useProjectArtifact(documentRoute).data?.name;
  const pageScope = scopeOfPage(location.pathname, location.search, documentName, selectedIssues);
  const scope =
    scopeLevel === "block" ? blockScope : scopeLevel === "project" ? projectScope : pageScope;
  // The person's recent items, read from the browser on the render that opens the bar: the
  // keystroke that opens it makes no request, which is what lets the list paint at once. They
  // are one person's own, so they wait for the signed-in identity the shell already read.
  const whoAmI = useQuery(whoAmIQuery());
  const [recents] = useState(() => readRecents(whoAmI.data?.login));
  // The rows this open offers, decided on its first render, while focus is still on whatever
  // opened the bar, since `useDialog` moves it into the input after that commit. Read any
  // later and a `when()` that asks what is focused would answer "the bar", withdrawing every
  // row bound to the card, row or control the reader was actually on.
  const [actions] = useState(() => appKeymap.actions());
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
  const tooShort = searchText.length < 2;
  // The server refuses a longer query; name the limit here instead of sending it.
  const tooLong = searchText.length > SEARCH_QUERY_MAX;
  const queryEnabled = !tooShort && !tooLong;

  useEffect(() => {
    const timeout = window.setTimeout(() => setDebouncedQuery(searchText), 150);
    return () => window.clearTimeout(timeout);
  }, [searchText]);

  // The chips narrow every leg of the search, so they belong to the request and to the key the
  // answer is kept under: a chip added or dropped is another search, not another reading of
  // this one. The page's project rides with them, under the parameter that narrows nothing.
  const request = searchRequest(filters, pageProject);
  const search = useQuery({
    enabled: debouncedQuery.length >= 2 && debouncedQuery.length <= SEARCH_QUERY_MAX,
    queryFn: () => api.search(debouncedQuery, request),
    queryKey: ["search", debouncedQuery, request],
  });
  // The project chip's own list, which every open of the bar offers, and the rows a query that
  // names a project leads to.
  const projects = useQuery(projectsQuery());
  const results = search.data?.results ?? emptyResults;
  // What search made of the words, read from its own answer rather than parsed a second time
  // here, and whether meaning search ran at all. A Dispatch that predates either field answers
  // neither, so the bar marks no operator and says nothing about meaning search.
  const operators = search.data?.query?.operators ?? emptyOperators;
  const degraded = search.data?.degraded !== undefined;
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
  // Projects are rows of the one bar, found by typing: `g p` opens the bar as every other key
  // does, and the project a reader is after is one or two letters away. With an empty box they
  // are not listed at all - every project of a Dispatch would bury the recent items and the
  // actions a reader opened the bar for.
  const projectRows: ProjectRow[] =
    needle === ""
      ? []
      : (projects.data ?? emptyProjects)
          .filter((project) => `${project.key} ${project.name}`.toLowerCase().includes(needle))
          .map((project) => ({
            id: `search-option-project-${project.key}`,
            kind: "project",
            project,
          }));
  // What the person opened lately, listed while the box is empty: with words in the box the
  // hits answer them, and a recent item among the hits would be the same item twice.
  const recentRows: RecentRow[] =
    searchText === ""
      ? recents.map((item) => ({ id: `search-option-recent-${item.path}`, item, kind: "recent" }))
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
  // Words the search answered and matched nothing with: the bar offers them as a new issue,
  // under the rows this page can do and the projects they name, and never above them - the
  // offer is the last thing left when nothing else answers.
  const newIssue = useNewIssueOffer({
    offered: showHits && results.length === 0,
    onOpen: (key) => {
      navigate(buildIssuePath({ key, kind: "issue" }));
      onClose();
    },
    project: pageProject,
    words: searchText,
  });
  // The words in the box as a message to the agent already on this issue, offered only while a
  // live session holds that issue's claim. It asks whatever the words are: the row is what
  // sends, and it sits among the foot rows below, so no Enter aimed at a hit sends anything.
  const sendToAgent = useSendToAgentOffer({
    issue: pageIssue,
    words: searchText,
  });
  const commandSections: readonly CommandSection[] = [
    {
      id: RECENT_GROUP_ID,
      label: "Recent",
      options: recentRows.map((row) => ({
        hint: null,
        label: row.item.label,
        row,
      })),
    },
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
  // The three sections at the foot of the list, which the highlight never starts on and the
  // arrows reach last: a More action is a coincidence of words, and each offer is a write, so
  // none of them runs on an Enter the reader meant for the row above. The two offers come
  // under the actions, because they are the rows that write.
  const newIssueSection: CommandSection = {
    id: NEW_ISSUE_GROUP_ID,
    label: "New issue",
    options:
      newIssue === undefined
        ? []
        : [
            {
              hint: null,
              label: newIssue.label,
              row: { id: NEW_ISSUE_OPTION_ID, kind: "new-issue" } as const,
            },
          ],
  };
  const sendToAgentSection: CommandSection = {
    id: SEND_TO_AGENT_GROUP_ID,
    label: "This issue's agent",
    options:
      sendToAgent?.row === undefined
        ? []
        : [
            {
              hint: null,
              label: sendToAgent.row.label,
              row: { id: SEND_TO_AGENT_OPTION_ID, kind: "send-to-agent" } as const,
            },
          ],
  };
  const moreActions: CommandSection = {
    id: MORE_ACTIONS_GROUP_ID,
    label: "More actions",
    options: actionOptions.filter((option) => !option.leads),
  };
  // The order the foot is listed and walked in, written once: the rows, the count the highlight
  // rule keeps off them, and the groups drawn below all read this one array.
  const footSections: readonly CommandSection[] = [
    moreActions,
    sendToAgentSection,
    newIssueSection,
  ];
  const footRows = footSections.reduce((count, section) => count + section.options.length, 0);
  // What the list shows, in the order it shows it: the arrows walk this same list.
  const rows: PaletteRow[] = [
    ...commandSections.flatMap((section) => section.options.map((option) => option.row)),
    ...hitGroups.flatMap((group) => group.rows),
    ...footSections.flatMap((section) => section.options.map((option) => option.row)),
  ];
  // The highlight is the row the reader sees highlighted, held by its id: hits arriving below
  // the actions, or a refetch re-ranking them, leave it on that row. A new query sends it to the
  // head, and so does its row leaving the list; the head it lands on is then adopted, so the row
  // coming back does not take the highlight back from it. The head is the first row unless that
  // is one of the foot rows - the two offers and the More actions, which come last: with
  // only those listed, nothing is highlighted, and Enter runs nothing until the reader arrows.
  const chosen = rows.findIndex((row) => row.id === activeId);
  const activeIndex = chosen === -1 && rows.length > footRows ? 0 : chosen;
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
    if (row.kind === "new-issue") {
      // One of the two rows that write: it makes the issue with the bar still open, so the
      // person reads what was made and, with no session named, that nothing was started on it.
      // It closes the bar itself, by opening the issue.
      newIssue?.select();
      return;
    }
    if (row.kind === "send-to-agent") {
      // The other: pressing it is the approval, and the approval is what sends. The bar stays
      // open, so the person reads that it went and to whom.
      sendToAgent?.row?.select();
      return;
    }
    if (row.kind === "action") {
      onAction(row.action.run);
    } else {
      navigate(
        row.kind === "project"
          ? buildProjectPath({ kind: "project", project: row.project.key })
          : row.kind === "recent"
            ? row.item.path
            : row.result.href
      );
    }
    onClose();
  };
  // The two keys that move the chip. `Tab` narrows it to the block the writing cursor was in,
  // and only where there was one: everywhere else Tab still steps through the bar's own
  // controls, which is the only way a reader without a pointer reaches the filter chips.
  // `Backspace` widens it to the project, and only on an empty box: with words in the box it
  // rubs out a letter, as it does in every other field.
  const canNarrow = blockScope !== undefined && scopeLevel !== "block";
  const canWiden = projectScope !== undefined && scopeLevel !== "project" && scope !== projectScope;
  const handleScopeKey = (event: KeyboardEvent<HTMLInputElement>): boolean => {
    if (event.key === "Tab" && !event.shiftKey && canNarrow) {
      setScopeLevel("block");
      return true;
    }
    if (event.key === "Backspace" && query === "" && canWiden) {
      setScopeLevel("project");
      return true;
    }
    return false;
  };
  // While the query's search is out - its first answer, or a refetch behind a cached one - its
  // hits can still arrive above the More actions, so an arrow aimed at the list now would land
  // on whatever row then sits there: until the search answers, the arrows walk only the rows
  // above the More actions, and with none they do nothing. A highlight the reader moved into the
  // More actions after an earlier answer keeps moving over the whole list. `isFetching`, not the
  // message's `isPending`: with a stale cached answer `isPending` is false while the refetch runs.
  const searchOut = waitingForQuery || (queryEnabled && search.isFetching);
  const firstMore = rows.length - footRows;
  const walkable = searchOut && activeIndex < firstMore ? firstMore : rows.length;
  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (handleScopeKey(event)) {
      // The browser's own move is what this cancels: the box is the dialog's first focusable,
      // so the dialog's Tab trap, which acts at either end, leaves this key alone.
      event.preventDefault();
    } else if ((event.key === "ArrowDown" || event.key === "ArrowUp") && walkable > 0) {
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
          {/* What the bar will act on, above the box, with the two keys that move it. The
              filter chips below the box are another thing and read as one: they say which items
              the words search, each with a Remove control of its own, while this says what the
              bar acts on and is moved by Tab and Backspace alone. */}
          {scope === undefined ? null : (
            <div
              className={`flex shrink-0 flex-wrap items-center gap-x-3 gap-y-1 border-b px-3 py-2 text-xs ${borderDefault} ${textSecondaryOnSurface}`}
              data-testid="search-scope"
              role="status"
            >
              <span className="min-w-0">
                Acting on <span className="font-medium">{scope}</span>
              </span>
              {canNarrow ? (
                <span className={`inline-flex items-center gap-1 ${textMutedOnSurface}`}>
                  <KeyHints keys={["Tab"]} />
                  for the block you are writing in
                </span>
              ) : null}
              {canWiden ? (
                <span className={`inline-flex items-center gap-1 ${textMutedOnSurface}`}>
                  <KeyHints keys={["Backspace"]} />
                  for the whole project
                </span>
              ) : null}
            </div>
          )}
          <div className={`shrink-0 border-b p-3 ${borderDefault}`}>
            <input
              aria-activedescendant={activeRow?.id}
              aria-controls="search-results"
              aria-expanded={rows.length > 0}
              aria-label="Search"
              className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
              onChange={(event) => {
                setQuery(event.target.value);
                // A new query re-ranks the list, so the highlight returns to its head.
                setActiveId(null);
              }}
              onKeyDown={handleKeyDown}
              placeholder="Search Dispatch"
              ref={inputRef}
              role="combobox"
              type="search"
              value={query}
            />
          </div>
          {/* What narrows the search and what the words themselves can do, between the box and
              the hits: the chips the reader has added with the controls for the rest, the three
              operator forms, and, where meaning search did not run, the one line saying so. */}
          <div className={`shrink-0 border-b ${borderDefault}`}>
            <SearchFilterBar
              filters={filters}
              onFilters={setFilters}
              projects={projects.data ?? emptyProjects}
            />
            <SearchOperators inUse={operators} />
            {degraded ? (
              <p
                className={`px-3 pb-2 text-xs ${textMutedOnSurface}`}
                data-testid="search-degraded"
                role="status"
              >
                Searching words only: meaning search is not running, so a hit that uses other words
                may be missing.
              </p>
            ) : null}
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
              {footSections.map((section) => (
                <CommandGroup
                  activeId={activeRow?.id}
                  key={section.id}
                  onSelect={selectRow}
                  section={section}
                />
              ))}
            </div>
          )}
          {/* What the bar says about what it sent and what it made from these words, above the
              message that says the search found none: each is an answer to that search. */}
          {sendToAgent?.said}
          {newIssue?.said}
          {tooShort ? (
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
          ) : search.data?.total !== undefined && search.data.total > results.length ? (
            // The bar lists one page of hits, so a query matching more says so rather than
            // looking complete.
            <p className={barNoteClassName}>
              Showing the best {results.length} of {search.data.total} matches
            </p>
          ) : projects.isError && needle !== "" ? (
            // Projects are rows of this list too, so a query that found none has to say
            // whether that is because the list of them never arrived.
            <div className="shrink-0 p-3">
              <QueryError
                message="Could not load projects."
                onRetry={() => {
                  void projects.refetch();
                }}
                retrying={projects.isFetching}
              />
            </div>
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
