import { type ReactNode, useCallback, useLayoutEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { ApiError } from "../../api/client";
import type { Artifact } from "../../api/types";
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
import { NotFoundPage } from "../shell/NotFoundPage";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { ChildrenTab } from "./ChildrenTab";
import { IssueHeader, stateForIssue } from "./IssueHeader";
import { IssueTabs } from "./IssueTabs";
import { SpecToolbar } from "./SpecToolbar";
import { useIssueDetail } from "./useIssueDetail";

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

  useDocumentTitle(
    issue.data === undefined ? "Dispatch" : `${issue.data.key} · ${issue.data.title} · Dispatch`
  );

  if (issue.isPending || state.isPending) {
    return <p className={textMutedOnCanvas}>Loading issue…</p>;
  }
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
  if (issue.data === undefined) {
    return <p className={dangerText}>Could not load this issue.</p>;
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
        state={issueState}
      />
      <IssueTabs
        activeTab={activeTab}
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
            focusItemId={conversationFocusItemId}
            isClosed={isClosed}
            issueKey={issueKey}
            route={issue.data.route}
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
