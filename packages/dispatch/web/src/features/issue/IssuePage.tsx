import { type ReactNode, useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

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
import { ActivityTab } from "../activity/ActivityTab";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import { ArtifactRoutePanel } from "../artifacts/ArtifactRoutePanel";
import { ConversationTab } from "../conversation/ConversationTab";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { useFillMarginSlot } from "../margin/margin-slot";
import { PlanTab } from "../plan/PlanTab";
import { statusText } from "../project/board-model";
import {
  buildIssuePath,
  buildReferencePath,
  documentRoute,
  type IssueRoute,
  type IssueTab,
  parseIssuePath,
} from "../refs/routes";
import { NotFoundPage } from "../shell/NotFoundPage";
import { type FrameAttributes, usePageFrame } from "../shell/page-frame";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { VerificationTab } from "../verification/VerificationTab";
import { narrowConversation } from "./conversation-states";
import { DecisionsPanel } from "./DecisionsPanel";
import { DocumentStage, DocumentStages, STAGE_IN_PANEL, useDocumentStage } from "./document-stage";
import { type FocusedNode, FocusedNodeProvider, useFocusedNode } from "./focused-node";
import { IssueBody } from "./IssueBody";
import { IssueHeader } from "./IssueHeader";
import { IssueMarginPanel } from "./IssueMargin";
import { issueAddress } from "./issue-address";
import { artifactsBySlug, documentOfNode } from "./issue-artifacts";
import { PresetToolbar } from "./PresetToolbar";
import { type ConversationState, NODE_TYPE_NAMES, sentenceCase } from "./page-words";
import { stateForIssue } from "./pins";
import { RoleDocumentPanel } from "./RoleDocumentPanel";
import { SpecRequirements } from "./SpecRequirements";
import { useIssueDetail } from "./useIssueDetail";
import { useIssueKeymap } from "./useIssueKeymap";
import { useIssuePriority } from "./useIssuePriority";
import { type ItemLanding, type ItemRoute, isItemRoute, useItemLanding } from "./useItemLanding";

/** The issue page's own root, which every browser check reads the page through
 *  (`e2e/issue-page.ts`): the shell's frame around this page's column and the margin beside it,
 *  so the identity block, the body and the panel's properties are one page. */
const ISSUE_PAGE_FRAME: FrameAttributes = { "data-issue-page": "" };

export function IssuePage(): ReactNode {
  const { pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  if (route === undefined) {
    return <NotFoundPage />;
  }
  // key={route.key}: switching to a different issue remounts IssueDetail
  // fresh (discarding any unsaved local drafts); switching tabs within the
  // same issue keeps route.key unchanged, so it only re-renders. The focus provider is above
  // it, so the page can read what the focus is on and hand it to the panel, which the shell's
  // margin draws outside this tree (`focused-node.tsx`).
  return (
    <FocusedNodeProvider key={route.key}>
      <DocumentStages>
        <IssueDetail route={route} />
      </DocumentStages>
    </FocusedNodeProvider>
  );
}

/** An item deep link the SPA could not resolve: the id names nothing on this issue, or the
 *  lookup failed. Either way the reader is told which item, inside the issue page. The item's
 *  name is the page's own word for that kind of node (`page-words.ts`). */
function ItemLandingFailure({
  landing,
  route,
}: {
  landing: Extract<ItemLanding, { kind: "missing" | "unavailable" }>;
  route: ItemRoute;
}): ReactNode {
  const noun = NODE_TYPE_NAMES[route.kind];
  if (landing.kind === "missing") {
    return (
      <NotFoundPage
        backLabel={`Back to ${route.key}`}
        backTo={`/issues/${route.key}`}
        detail={`That link doesn't name ${route.kind === "ask" ? "an" : "a"} ${noun} on ${route.key} — it may have been deleted.`}
        title={`${sentenceCase(noun)} not found`}
      />
    );
  }
  return (
    <section>
      <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>
        Couldn&apos;t load this {noun}
      </h1>
      <div className="mt-2">
        <QueryError message={`Couldn't load this ${noun}.`} onRetry={landing.retry} />
      </div>
    </section>
  );
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
    planArtifact,
    selectedArtifact,
    state,
    verificationArtifact,
  } = useIssueDetail(route);
  const landing = useItemLanding(route, issue.data);
  const { hash, pathname, search } = useLocation();
  const navigate = useNavigate();
  const panelScroll = useRef<Partial<Record<IssueTab, number>>>({});
  // One toolbar and one diff per document, by its artifact id: the page draws the toolbar of
  // whichever document the preset has open (R16), and every editor on the page reports its own
  // through a callback of its own, held across renders because the editor publishes it from an
  // effect.
  const [toolbars, setToolbars] = useState<ReadonlyMap<string, DocumentToolbar>>(() => new Map());
  const [diffs, setDiffs] = useState<ReadonlySet<string>>(() => new Set());
  const sinks = useRef(new Map<string, (next: DocumentToolbar | undefined) => void>());
  const showDiff = useCallback(
    (artifactId: string, next: boolean) =>
      setDiffs((held) => {
        const made = new Set(held);
        if (next) made.add(artifactId);
        else made.delete(artifactId);
        return made;
      }),
    []
  );
  const toolbarSink = useCallback(
    (artifactId: string) => {
      const held = sinks.current.get(artifactId);
      if (held !== undefined) return held;
      const made = (next: DocumentToolbar | undefined) => {
        setToolbars((toolbars) => {
          const replaced = new Map(toolbars);
          if (next === undefined) replaced.delete(artifactId);
          else replaced.set(artifactId, next);
          return replaced;
        });
        // A document that has closed shows no diff: the control that turned one on has gone
        // with it.
        if (next === undefined) showDiff(artifactId, false);
      };
      sinks.current.set(artifactId, made);
      return made;
    },
    [showDiff]
  );
  // The document a row of the tree is being edited in, so the page draws its one editor for
  // that row to take onto its own stage (`document-stage.tsx`).
  const [editing, setEditing] = useState<string | undefined>(undefined);
  const specStage = useDocumentStage(primaryArtifact?.id, STAGE_IN_PANEL);
  const verificationStage = useDocumentStage(verificationArtifact?.id, STAGE_IN_PANEL);
  const planStage = useDocumentStage(planArtifact?.id, STAGE_IN_PANEL);
  const selectedStage = useDocumentStage(selectedArtifact?.id, STAGE_IN_PANEL);
  // What this address opens: its preset, the node it names, the conversation's state, the
  // filter and the version it pins (`issue-address.ts`).
  const address = issueAddress(route, search, hash, isPrimaryArtifactRoute);
  // Which state the conversation is in: the address names one, and a state the reader picked
  // in the panel holds for the address it navigated to and for no other, so opening another
  // address is back to what that address says.
  const [picked, setPicked] = useState<{ at: string; state: ConversationState } | undefined>(
    undefined
  );
  const conversationState = picked?.at === pathname ? picked.state : address.conversation;

  // The issue page's own root is the frame the shell draws around this column and the margin
  // beside it: the identity block, the body and the panel's properties are one page, however the
  // shell mounts them (`features/shell/page-frame.tsx`, `e2e/issue-page.ts`).
  usePageFrame(ISSUE_PAGE_FRAME);
  // Where the panel goes: the shell's margin at the width that has one, and the page's own
  // column under the identity block below it, where the margin is a sheet and the properties
  // would otherwise be behind it (R11, R12).
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const details = issue.data;
  const focusedNode = useFocusedNode();
  // Nothing has the focus until the tree lands, and a reader on no node is reading the issue,
  // so the issue itself is what the panel draws the facts of. Held across renders, because the
  // panel the margin draws is registered under this value.
  const focused = useMemo<FocusedNode>(
    () =>
      focusedNode ?? {
        id: route.key,
        state: details === undefined ? undefined : statusText(details.status),
        title: details?.title ?? route.key,
        type: "issue",
      },
    [details, focusedNode, route.key]
  );
  // Where the reader was when they left this address, taken before the move: the tablist used
  // to say so before it changed tab (`onBeforeTabChange`), and the presets are navigation, so
  // every control that leaves an address says so first. Taken afterwards it would be the
  // position the page had already clamped to as the panel it is leaving went.
  const recordScroll = useCallback(() => {
    panelScroll.current[activeTab] = window.scrollY;
  }, [activeTab]);

  const pickState = useCallback(
    (next: ConversationState) => {
      const to = buildIssuePath({
        key: route.key,
        kind: next === "everything" ? "activity" : "conversation",
      });
      setPicked({ at: to, state: next });
      recordScroll();
      navigate(to);
    },
    [navigate, recordScroll, route.key]
  );
  const panel = useCallback(
    (conversation: ReactNode) =>
      details === undefined ? (
        conversation
      ) : (
        <IssueMarginPanel
          conversation={conversation}
          conversationState={conversationState}
          focused={focused}
          isClosed={details.closed_at !== null}
          issue={details}
          onConversationState={pickState}
        />
      ),
    [conversationState, details, focused, pickState]
  );
  useFillMarginSlot(isCompactViewport ? undefined : panel);

  // The Conversation's first placement runs as a child layout effect, before the restore below
  // in the same commit, so a position recorded while the tab showed only its loading text is
  // forgotten here and the reader lands on the newest message. A return to a list the reader was
  // already placed in never fires it, so that restore stands (INFRA-303).
  const forgetConversationScroll = useCallback(() => {
    delete panelScroll.current.conversation;
  }, []);
  // Every key the issue page binds, in its own scope (`useIssueKeymap.ts`). The priority write
  // is the page's one, shared with the identity block and the palette, so a refusal of any of
  // them is reported once.
  const priorityWrite = useIssuePriority(route.key);
  useIssueKeymap(priorityWrite, route.key);

  // Where the reader left this tab, which this commit is about to put them back at. It is read
  // here, in the render, and not only in the effect below, because the identity block's two
  // forms are 26 px apart in flow height: the block is told the place first (`restoringTo`), so
  // its height is final before the scroll is written and the browser anchors nothing away.
  const restoringTo = panelScroll.current[activeTab];

  useLayoutEffect(() => {
    const top = panelScroll.current[activeTab];
    if (top === undefined) return;
    // Taken, so the next render of this tab reads the page's own position again rather than a
    // place the reader has since left; leaving the tab records it afresh.
    delete panelScroll.current[activeTab];
    window.scrollTo({ top });
  }, [activeTab]);

  // Once a panel's tab has ever been active, keep rendering its content even
  // while hidden — that is what keeps the Spec document connected and the Conversation's read
  // observer alive across tab switches (see the `hidden` panels below). A panel the user has
  // never opened stays unmounted, so a fresh page load doesn't pay for panels it never shows.
  const [activatedTabs, setActivatedTabs] = useState<Record<IssueTab, boolean>>(() => ({
    activity: activeTab === "activity",
    artifacts: activeTab === "artifacts",
    children: activeTab === "children",
    conversation: activeTab === "conversation",
    decisions: activeTab === "decisions",
    plan: activeTab === "plan",
    spec: activeTab === "spec",
    verification: activeTab === "verification",
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
  // Which version of the spec the page reads: the version a `/artifacts/<spec>` route carries,
  // and the `?v=` the issue's own `/spec` address holds, which is the address a reference to a
  // version of the spec resolves to (R19, c-urls).
  const specVersion = address.preset === "spec" ? address.version : undefined;
  // Whose toolbar the preset row draws: the document the preset has open, and on the Tree and
  // Decisions presets, which draw nodes of every document, the focused node's own (R16). A
  // focus on a node that belongs to no document leaves the row with no toolbar at all.
  const openDocument =
    address.preset === "spec"
      ? primaryArtifact
      : address.preset === "checks"
        ? verificationArtifact
        : address.preset === "plan"
          ? planArtifact
          : documentOfNode(focused.id, artifactsBySlug(issue.data))?.artifact;
  const openToolbar = openDocument === undefined ? undefined : toolbars.get(openDocument.id);
  const openVersion = openDocument?.id === primaryArtifact.id ? specVersion : undefined;
  // The item an address names (`?ask=`, `?comment=`) is handed to the spec's editor only where
  // the Spec preset is what the address opens. On every other preset the document is content
  // beside the tree, and the tree is what the address names (R19): an editor that focused the
  // item there would take the focus off the row the reader was sent to.
  // The document a row is editing, where the page draws it nowhere else: one editor per
  // document and never a second, so a row of a document no panel has open gets one here.
  const drawnDocuments = new Set(
    [
      activatedTabs.spec && !(artifactRoute !== undefined && !isPrimaryArtifactRoute)
        ? primaryArtifact.id
        : undefined,
      activatedTabs.verification ? verificationArtifact?.id : undefined,
      activatedTabs.plan ? planArtifact?.id : undefined,
      activatedTabs.artifacts &&
      artifactRoute !== undefined &&
      !isPrimaryArtifactRoute &&
      selectedArtifact?.kind === "doc"
        ? selectedArtifact.id
        : undefined,
    ].filter((id): id is string => id !== undefined)
  );
  const editedDocument =
    editing === undefined || drawnDocuments.has(editing)
      ? undefined
      : issue.data.artifacts.find((artifact) => artifact.id === editing);

  return (
    <section>
      <IssueHeader
        documentArtifact={primaryArtifact}
        isClosed={isClosed}
        issue={issue.data}
        priorityWrite={priorityWrite}
        restoringTo={restoringTo}
        state={issueState}
      />
      {/* The head of the page, the preset row, the children section and the one folded tree
          the presets are queries over (R1, R3, R5, R10), on every address. A preset's nodes
          are the tree's; the panels below hold the documents those nodes live in, and a row
          being edited takes its own document onto its stage (`document-stage.tsx`). */}
      <IssueBody
        address={address}
        isClosed={isClosed}
        issue={issue.data}
        onEditing={setEditing}
        onLeave={recordScroll}
        plan={planArtifact}
        toolbar={
          openDocument !== undefined && openToolbar !== undefined ? (
            <PresetToolbar
              artifact={openDocument}
              isClosed={isClosed}
              onShowDiffChange={(next) => showDiff(openDocument.id, next)}
              onVersionChange={(version) => {
                showDiff(openDocument.id, false);
                selectDocumentVersion(openDocument, version);
              }}
              reference={documentRoute(openDocument, openVersion)}
              showDiff={diffs.has(openDocument.id)}
              toolbar={openToolbar}
              version={openVersion}
            />
          ) : undefined
        }
      />
      {/* Below the width the shell's margin keeps a column at, the panel is drawn here, under
          the identity block, and the margin becomes the phone sheet that holds the
          conversation: the properties are read at every width with nothing opened (R11, R12). */}
      {isCompactViewport ? (
        <div className="mb-3">
          <IssueMarginPanel
            conversationState={conversationState}
            focused={focused}
            isClosed={isClosed}
            issue={issue.data}
            onConversationState={pickState}
          />
        </div>
      ) : null}
      {/* What each preset draws beside the tree. These are plain panels, not tabpanels: the
          presets are queries over one tree and the page has no tablist of panels any more. */}
      <div aria-hidden={activeTab !== "spec"} hidden={activeTab !== "spec"} id="issue-spec-panel">
        {activatedTabs.spec && !(artifactRoute !== undefined && !isPrimaryArtifactRoute) ? (
          <div className="space-y-4">
            <div className="flex min-w-0 flex-col" ref={specStage} />
            <DocumentStage artifactId={primaryArtifact.id}>
              <ArtifactDocument
                artifact={primaryArtifact}
                askId={address.preset === "spec" ? askId : undefined}
                commentId={address.preset === "spec" ? commentId : undefined}
                highlightTerm={highlightTerm}
                isClosed={isClosed}
                onToolbarChange={toolbarSink(primaryArtifact.id)}
                owner={{ key: issueKey, kind: "issue" }}
                onVersionChange={(version) => {
                  showDiff(primaryArtifact.id, false);
                  selectDocumentVersion(primaryArtifact, version);
                }}
                showDiff={diffs.has(primaryArtifact.id)}
                version={specVersion}
              />
            </DocumentStage>
            <SpecRequirements issueKey={issueKey} />
          </div>
        ) : null}
      </div>
      <RoleDocumentPanel
        Tab={VerificationTab}
        activated={activatedTabs.verification}
        active={activeTab === "verification"}
        artifact={verificationArtifact}
        isClosed={isClosed}
        issueKey={issueKey}
        onToolbarChange={toolbarSink}
        onVersionChange={selectDocumentVersion}
        showDiff={diffs}
        stage={verificationStage}
        tab="verification"
      />
      <RoleDocumentPanel
        Tab={PlanTab}
        activated={activatedTabs.plan}
        active={activeTab === "plan"}
        artifact={planArtifact}
        isClosed={isClosed}
        issueKey={issueKey}
        onToolbarChange={toolbarSink}
        onVersionChange={selectDocumentVersion}
        showDiff={diffs}
        stage={planStage}
        tab="plan"
      />
      <div
        aria-hidden={activeTab !== "conversation"}
        hidden={activeTab !== "conversation"}
        id="issue-conversation-panel"
      >
        {activatedTabs.conversation ? (
          <ConversationTab
            issueArtifacts={issueArtifacts}
            focusItemId={conversationFocusItemId}
            isClosed={isClosed}
            issueKey={issueKey}
            narrow={narrowConversation(conversationState, issueState.dismissed)}
            onFirstPlacement={forgetConversationScroll}
            state={state.data}
            visible={activeTab === "conversation"}
          />
        ) : null}
      </div>
      <ArtifactRoutePanel
        active={activeTab === "artifacts"}
        artifact={selectedArtifact}
        artifactRoute={artifactRoute}
        isClosed={isClosed}
        isPrimaryArtifactRoute={isPrimaryArtifactRoute}
        mounted={activatedTabs.artifacts}
        onShowDiffChange={(next) => showDiff(selectedArtifact?.id ?? "", next)}
        route={route}
        showDiff={diffs.has(selectedArtifact?.id ?? "")}
        toolbar={toolbars.get(selectedArtifact?.id ?? "")}
      >
        {selectedArtifact?.kind === "doc" ? (
          <>
            <div className="flex min-w-0 flex-col" ref={selectedStage} />
            <DocumentStage artifactId={selectedArtifact.id}>
              <ArtifactDocument
                artifact={selectedArtifact}
                askId={askId}
                commentId={commentId}
                highlightTerm={highlightTerm}
                isClosed={isClosed}
                onToolbarChange={toolbarSink(selectedArtifact.id)}
                owner={{ key: issueKey, kind: "issue" }}
                onVersionChange={(version) => {
                  showDiff(selectedArtifact.id, false);
                  selectDocumentVersion(selectedArtifact, version);
                }}
                showDiff={diffs.has(selectedArtifact.id)}
                version={artifactRoute?.version}
              />
            </DocumentStage>
          </>
        ) : null}
      </ArtifactRoutePanel>
      {/* The document a row of the tree is being edited in, where no panel above draws it:
          one editor per document, so the row takes this one onto its own stage. */}
      {editedDocument === undefined ? null : (
        <DocumentStage artifactId={editedDocument.id}>
          <ArtifactDocument
            artifact={editedDocument}
            isClosed={isClosed}
            onToolbarChange={toolbarSink(editedDocument.id)}
            owner={{ key: issueKey, kind: "issue" }}
            onVersionChange={(version) => selectDocumentVersion(editedDocument, version)}
            showDiff={false}
            version={undefined}
          />
        </DocumentStage>
      )}
      <div
        aria-hidden={activeTab !== "activity"}
        hidden={activeTab !== "activity"}
        id="issue-activity-panel"
      >
        {activatedTabs.activity ? <ActivityTab issueKey={issueKey} state={state.data} /> : null}
      </div>
      <div
        aria-hidden={activeTab !== "decisions"}
        hidden={activeTab !== "decisions"}
        id="issue-decisions-panel"
      >
        {activatedTabs.decisions ? (
          <DecisionsPanel
            artifacts={issue.data?.artifacts ?? []}
            isClosed={isClosed}
            issueKey={issueKey}
          />
        ) : null}
      </div>
    </section>
  );
}
