import { type ReactNode, useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { ApiError } from "../../api/client";
import type { Artifact, IssuePriority } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import { ArtifactRoutePanel } from "../artifacts/ArtifactRoutePanel";
import { ConversationTab } from "../conversation/ConversationTab";
import type { DocumentToolbar } from "../doc/ProofDocument";
import {
  buildReferencePath,
  documentRoute,
  type IssueRoute,
  type IssueTab,
  parseIssuePath,
} from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { ChildrenTab } from "./ChildrenTab";
import { IssueHeader } from "./IssueHeader";
import { IssueTabs } from "./IssueTabs";
import { stateForIssue } from "./pins";
import { SpecToolbar } from "./SpecToolbar";
import { useIssueDetail } from "./useIssueDetail";
import { useIssuePriority } from "./useIssuePriority";
import { type ItemLanding, type ItemRoute, isItemRoute, useItemLanding } from "./useItemLanding";

export function IssuePage(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) {
    return <NotFoundPage />;
  }
  // key={route.key}: switching to a different issue remounts IssueDetail
  // fresh (discarding any unsaved local drafts); switching tabs within the
  // same issue keeps route.key unchanged, so it only re-renders.
  return <IssueDetail key={route.key} route={route} />;
}

const itemNoun: Record<ItemRoute["kind"], string> = {
  ask: "Ask",
  comment: "Comment",
  message: "Message",
};

/** An item deep link the SPA could not resolve: the id names nothing on this issue, or the
 *  lookup failed. Either way the reader is told which item, inside the issue page. */
function ItemLandingFailure({
  landing,
  route,
}: {
  landing: Extract<ItemLanding, { kind: "missing" | "unavailable" }>;
  route: ItemRoute;
}): ReactNode {
  const noun = itemNoun[route.kind];
  if (landing.kind === "missing") {
    return (
      <NotFoundPage
        backLabel={`Back to ${route.key}`}
        backTo={`/issues/${route.key}`}
        detail={`That link doesn't name ${route.kind === "ask" ? "an" : "a"} ${noun.toLowerCase()} on ${route.key} — it may have been deleted.`}
        title={`${noun} not found`}
      />
    );
  }
  return (
    <section>
      <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>
        Couldn&apos;t load this {noun.toLowerCase()}
      </h1>
      <div className="mt-2">
        <QueryError message={`Couldn't load this ${noun.toLowerCase()}.`} onRetry={landing.retry} />
      </div>
    </section>
  );
}

/**
 * What the `issue` scope acts on. Every target is a control the header or the tablist already
 * renders, so a key does exactly what a click on it does. Scoping the header lookup to
 * `issue-header` keeps a child issue's row in the Children tab - it carries the same accessible
 * names - out of reach.
 */
const LABELS_TRIGGER = 'button[aria-label="Edit labels"]';
const PIN_TOGGLE = 'button[aria-label="Pin issue"], button[aria-label="Unpin issue"]';
const PRIORITY_SELECT = 'select[aria-label^="Priority of "]';
const STATUS_SELECT = 'select[aria-label="Status"]';
const TITLE_HEADING = 'h1[tabindex="0"]';

/** The header control `selector` names, or `null` when it is absent or refuses input (a closed
 *  issue, a save in flight), so `?` offers a shortcut exactly while its control takes a click. */
function headerControl(selector: string): HTMLElement | null {
  const node = document
    .querySelector("[data-testid=issue-header]")
    ?.querySelector<HTMLElement>(selector);
  if (node == null) {
    return null;
  }
  const disabled =
    (node instanceof HTMLButtonElement || node instanceof HTMLSelectElement) && node.disabled;
  return disabled ? null : node;
}

/** The tab chords `IssueTabs` answers, keyed by the tablist button id `components/Tabs.tsx`
 *  gives each tab. */
const tabChords: readonly { id: string; keys: string; label: string; tab: IssueTab }[] = [
  { id: "tab-spec", keys: "t s", label: "Spec tab", tab: "spec" },
  { id: "tab-conversation", keys: "t c", label: "Conversation tab", tab: "conversation" },
  { id: "tab-children", keys: "t h", label: "Children tab", tab: "children" },
  { id: "tab-artifacts", keys: "t a", label: "Artifacts tab", tab: "artifacts" },
];

function IssueDetail({ route }: { route: IssueRoute }): ReactNode {
  const {
    activeTab,
    artifactRoute,
    askId,
    commentId,
    conversationFocusItemId,
    isPrimaryArtifactRoute,
    highlightTerm,
    issue,
    primaryArtifact,
    selectedArtifact,
    state,
  } = useIssueDetail(route);
  const landing = useItemLanding(route, issue.data);
  const navigate = useNavigate();
  const panelScroll = useRef<Partial<Record<IssueTab, number>>>({});
  const [specShowDiff, setSpecShowDiff] = useState(false);
  const [specToolbar, setSpecToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const [artifactShowDiff, setArtifactShowDiff] = useState(false);
  const [artifactToolbar, setArtifactToolbar] = useState<DocumentToolbar | undefined>(undefined);
  const handleSpecToolbarChange = useCallback((next: DocumentToolbar | undefined) => {
    setSpecToolbar(next);
    if (next === undefined) {
      setSpecShowDiff(false);
    }
  }, []);
  const handleArtifactToolbarChange = useCallback((next: DocumentToolbar | undefined) => {
    setArtifactToolbar(next);
    if (next === undefined) {
      setArtifactShowDiff(false);
    }
  }, []);

  // The scope is the issue page itself, and every binding drives the header control or the tab
  // its label names - the same handler a click reaches, offered while that control is. The
  // digits go through `useIssuePriority` on the route's key, the query key this page reads, so
  // a keyed priority lands in the same cache the picker writes.
  const priority = useIssuePriority(route.key);
  useKeymapScope("issue");
  useKeymap("issue", [
    {
      id: "status",
      keys: "s",
      label: "Focus the status",
      run: () => headerControl(STATUS_SELECT)?.focus(),
      when: () => headerControl(STATUS_SELECT) !== null,
    },
    {
      id: "priority",
      keys: "p",
      label: "Focus the priority",
      run: () => headerControl(PRIORITY_SELECT)?.focus(),
      when: () => headerControl(PRIORITY_SELECT) !== null,
    },
    {
      id: "set-priority",
      keys: ["0", "1", "2", "3"],
      label: "Set priority P0–P3",
      run: (event) => priority.submit(Number(event.key) as IssuePriority),
      when: () => headerControl(PRIORITY_SELECT) !== null,
    },
    {
      id: "labels",
      keys: "l",
      label: "Edit labels",
      run: () => headerControl(LABELS_TRIGGER)?.click(),
      when: () => headerControl(LABELS_TRIGGER) !== null,
    },
    {
      // The heading takes focus only while the issue is open: closed, its `tabIndex` is -1 and
      // `onFocus` does not open the editor, so the key is offered exactly while it edits.
      id: "title",
      keys: "e",
      label: "Edit the title",
      run: () => headerControl(TITLE_HEADING)?.focus(),
      when: () => headerControl(TITLE_HEADING) !== null,
    },
    {
      id: "pin",
      keys: "Shift+P",
      label: "Pin or unpin the issue",
      run: () => headerControl(PIN_TOGGLE)?.click(),
      when: () => headerControl(PIN_TOGGLE) !== null,
    },
    ...tabChords.map(({ id, keys, label, tab }) => ({
      id,
      keys,
      label,
      run: () => document.getElementById(`issue-${tab}-tab`)?.click(),
      when: () => document.getElementById(`issue-${tab}-tab`) !== null,
    })),
  ]);

  useLayoutEffect(() => {
    const top = panelScroll.current[activeTab];
    if (top !== undefined) {
      window.scrollTo({ top });
    }
  }, [activeTab]);

  // Once a panel's tab has ever been active, keep rendering its content even
  // while hidden — that is what keeps the Spec document connected and the Conversation's read
  // observer alive across tab switches (see the `hidden` panels below). A panel the user has
  // never opened stays unmounted, so a fresh page load doesn't pay for panels it never shows.
  const [activatedTabs, setActivatedTabs] = useState<Record<IssueTab, boolean>>(() => ({
    artifacts: activeTab === "artifacts",
    children: activeTab === "children",
    conversation: activeTab === "conversation",
    spec: activeTab === "spec",
  }));
  if (!activatedTabs[activeTab]) {
    setActivatedTabs({ ...activatedTabs, [activeTab]: true });
  }
  const issueArtifacts = useMemo(
    () => new Map((issue.data?.artifacts ?? []).map((artifact) => [artifact.id, artifact])),
    [issue.data?.artifacts]
  );

  useDocumentTitle(
    issue.data === undefined ? "Dispatch" : `${issue.data.key} · ${issue.data.title} · Dispatch`
  );

  // A failed issue read is reported before an item link's landing: the landing waits on the
  // issue to tell it which artifact an anchor names, so reading its pending state first turns a
  // 500 into "Loading issue…" for good.
  if (issue.isError || state.isError) {
    const notFound = issue.error instanceof ApiError && issue.error.status === 404;
    return (
      <section>
        <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>
          {notFound ? "Issue not found" : "Couldn't load this issue"}
        </h1>
        {notFound ? (
          <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
            {route.key} doesn&apos;t exist, or you don&apos;t have access to it.
          </p>
        ) : (
          <div className="mt-2">
            <QueryError
              message="Couldn't load this issue."
              onRetry={() => {
                void issue.refetch();
                void state.refetch();
              }}
            />
          </div>
        )}
        <Link
          className={`mt-4 inline-block text-sm font-medium underline ${linkText} ${linkHoverText}`}
          to="/"
        >
          Back to inbox
        </Link>
      </section>
    );
  }
  if (issue.isPending || state.isPending || landing?.kind === "pending") {
    return <p className={textMutedOnCanvas}>Loading issue…</p>;
  }
  if (issue.data === undefined) {
    return <p className={dangerText}>Could not load this issue.</p>;
  }
  if (landing?.kind === "redirect") {
    return <Navigate replace to={landing.to} />;
  }
  if (isItemRoute(route) && (landing?.kind === "missing" || landing?.kind === "unavailable")) {
    return <ItemLandingFailure landing={landing} route={route} />;
  }

  if (primaryArtifact === undefined) {
    return <p className={dangerText}>Could not load this issue&apos;s primary document.</p>;
  }

  const issueState = stateForIssue(state.data, issue.data.key);
  const isClosed = issue.data.closed_at !== null;
  const issueKey = issue.data.key;
  const selectDocumentVersion = (artifact: Artifact, version: number | null) =>
    navigate(buildReferencePath(documentRoute(artifact, version ?? undefined)));

  return (
    <section>
      <IssueHeader
        documentArtifact={primaryArtifact}
        isClosed={isClosed}
        issue={issue.data}
        priorityWrite={priority}
        state={issueState}
      />
      <IssueTabs
        activeTab={activeTab}
        artifactCount={issue.data.artifacts.length}
        issueKey={issueKey}
        onBeforeTabChange={(current) => {
          panelScroll.current[current] = window.scrollY;
        }}
        toolbar={
          activeTab === "spec" && specToolbar !== undefined ? (
            <SpecToolbar
              isClosed={isClosed}
              onShowDiffChange={setSpecShowDiff}
              onVersionChange={(version) => {
                setSpecShowDiff(false);
                selectDocumentVersion(primaryArtifact, version);
              }}
              reference={documentRoute(
                primaryArtifact,
                isPrimaryArtifactRoute ? artifactRoute?.version : undefined
              )}
              showDiff={specShowDiff}
              toolbar={specToolbar}
              version={isPrimaryArtifactRoute ? artifactRoute?.version : undefined}
            />
          ) : undefined
        }
      />
      <div
        aria-hidden={activeTab !== "spec"}
        aria-labelledby="issue-spec-tab"
        hidden={activeTab !== "spec"}
        id="issue-spec-panel"
        role="tabpanel"
      >
        {activatedTabs.spec && !(artifactRoute !== undefined && !isPrimaryArtifactRoute) ? (
          <ArtifactDocument
            artifact={primaryArtifact}
            askId={askId}
            commentId={commentId}
            highlightTerm={highlightTerm}
            isClosed={isClosed}
            onToolbarChange={handleSpecToolbarChange}
            owner={{ key: issueKey, kind: "issue" }}
            onVersionChange={(version) => {
              setSpecShowDiff(false);
              selectDocumentVersion(primaryArtifact, version);
            }}
            showDiff={specShowDiff}
            version={isPrimaryArtifactRoute ? artifactRoute?.version : undefined}
          />
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "conversation"}
        aria-labelledby="issue-conversation-tab"
        hidden={activeTab !== "conversation"}
        id="issue-conversation-panel"
        role="tabpanel"
      >
        {activatedTabs.conversation ? (
          <ConversationTab
            issueArtifacts={issueArtifacts}
            focusItemId={conversationFocusItemId}
            isClosed={isClosed}
            issueKey={issueKey}
            state={state.data}
            visible={activeTab === "conversation"}
          />
        ) : null}
      </div>
      <div
        aria-hidden={activeTab !== "children"}
        aria-labelledby="issue-children-tab"
        hidden={activeTab !== "children"}
        id="issue-children-panel"
        role="tabpanel"
      >
        {activatedTabs.children ? <ChildrenTab issue={issue.data} /> : null}
      </div>
      <ArtifactRoutePanel
        active={activeTab === "artifacts"}
        artifact={selectedArtifact}
        artifactRoute={artifactRoute}
        isClosed={isClosed}
        isPrimaryArtifactRoute={isPrimaryArtifactRoute}
        mounted={activatedTabs.artifacts}
        onShowDiffChange={setArtifactShowDiff}
        route={route}
        showDiff={artifactShowDiff}
        toolbar={artifactToolbar}
      >
        {selectedArtifact?.kind === "doc" ? (
          <ArtifactDocument
            artifact={selectedArtifact}
            askId={askId}
            commentId={commentId}
            highlightTerm={highlightTerm}
            isClosed={isClosed}
            onToolbarChange={handleArtifactToolbarChange}
            owner={{ key: issueKey, kind: "issue" }}
            onVersionChange={(version) => {
              setArtifactShowDiff(false);
              selectDocumentVersion(selectedArtifact, version);
            }}
            showDiff={artifactShowDiff}
            version={artifactRoute?.version}
          />
        ) : null}
      </ArtifactRoutePanel>
    </section>
  );
}
