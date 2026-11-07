import {
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, Navigate, useLocation, useNavigate } from "react-router-dom";

import { ApiError } from "../../api/client";
import type { Artifact } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { NoTooltips } from "../../components/tooltips";
import {
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ConversationTab } from "../conversation/ConversationTab";
import { conversationScrollport } from "../conversation/use-follow-latest";
import { useFillMarginSlot } from "../margin/margin-slot";
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
import { ISSUE_FRAME, usePageFrame } from "../shell/page-frame";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { narrowConversation } from "./conversation-states";
import { DocumentStages, STAGE_IN_PANEL, useDocumentStage } from "./document-stage";
import { type FocusedNode, FocusedNodeProvider, useFocusedNode } from "./focused-node";
import { IssueBody } from "./IssueBody";
import { IssueHeader } from "./IssueHeader";
import { IssueMarginPanel } from "./IssueMargin";
import { IssuePresetPanels } from "./IssuePresetPanels";
import { issueAddress } from "./issue-address";
import { artifactsBySlug, documentOfNode } from "./issue-artifacts";
import { PresetToolbar } from "./PresetToolbar";
import { type ConversationState, NODE_TYPE_NAMES, sentenceCase } from "./page-words";
import { stateForIssue } from "./pins";
import { useDocumentToolbars } from "./useDocumentToolbars";
import { useIssueDetail } from "./useIssueDetail";
import { useIssueKeymap } from "./useIssueKeymap";
import { useIssuePriority } from "./useIssuePriority";
import { type ItemLanding, type ItemRoute, isItemRoute, useItemLanding } from "./useItemLanding";

/* The issue page's own root is `ISSUE_FRAME` (`features/shell/page-frame.tsx`), which the
   shell also marks from the address alone until this page registers it, so a reload lands on
   the page rather than on a column with no root around it. */

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

/** How long after a Back or Forward the browser's own scroll restoration is still arriving, so
 *  the page leaves the window where that reader asked for it rather than zeroing it. */
const POP_RESTORES_MS = 100;

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
  const { diffs, showDiff, toolbarSink, toolbars } = useDocumentToolbars();
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
  usePageFrame(ISSUE_FRAME);
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
  // The Conversation is drawn in the page's one right-hand panel, which is a scrollport of its
  // own wherever the margin keeps a column, so where the reader was in it is that box's scroll
  // and not the window's (`conversation/use-follow-latest.ts`).
  const recordScroll = useCallback(() => {
    const port = activeTab === "conversation" ? conversationScrollport() : undefined;
    panelScroll.current[activeTab] = port === undefined ? window.scrollY : port.scrollTop;
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
  // The Conversation's first placement runs as a child layout effect, before the restore below
  // in the same commit, so a position recorded while the tab showed only its loading text is
  // forgotten here and the reader lands on the newest message. A return to a list the reader was
  // already placed in never fires it, so that restore stands (INFRA-303).
  const forgetConversationScroll = useCallback(() => {
    delete panelScroll.current.conversation;
  }, []);
  const issueArtifacts = useMemo(
    () => new Map((issue.data?.artifacts ?? []).map((artifact) => [artifact.id, artifact])),
    [issue.data?.artifacts]
  );
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
  // The conversation's one list, which the panel draws under the four state controls that
  // drive it (R12): it belongs to the panel and to no column of the page, and it is drawn
  // wherever the panel is. It is mounted once its own address has been opened and kept
  // mounted after that, as every other panel of the page is (`activatedTabs`): the margin's
  // own cards are already on the page, so a second list of the same comments at every address
  // would double the page's controls and its first load's reads. Whether the reader is *in*
  // it - what places them at the newest card, what marks a card read, what a deep link
  // scrolls to - is the address's own answer (`visible`).
  const conversationList = useMemo(
    () =>
      details === undefined || !activatedTabs.conversation ? undefined : (
        <ConversationTab
          issueArtifacts={issueArtifacts}
          focusItemId={conversationFocusItemId}
          isClosed={details.closed_at !== null}
          issueKey={details.key}
          narrow={narrowConversation(
            conversationState,
            stateForIssue(state.data, details.key).dismissed
          )}
          onFirstPlacement={forgetConversationScroll}
          state={state.data}
          visible={activeTab === "conversation"}
        />
      ),
    [
      activatedTabs.conversation,
      activeTab,
      conversationFocusItemId,
      conversationState,
      details,
      forgetConversationScroll,
      issueArtifacts,
      state.data,
    ]
  );
  const panel = useCallback(
    (conversation: ReactNode) =>
      details === undefined ? (
        conversation
      ) : (
        <IssueMarginPanel
          conversation={conversation}
          conversationList={conversationList}
          conversationState={conversationState}
          focused={focused}
          isClosed={details.closed_at !== null}
          issue={details}
          onConversationState={pickState}
        />
      ),
    [conversationList, conversationState, details, focused, pickState]
  );
  // Below the width the margin keeps a column at, the page's panel is drawn in its own column
  // and the margin's sheet keeps only its own cards - which are still this page's, so they
  // still draw no tooltips (`components/tooltips.tsx`).
  const hush = useCallback((content: ReactNode) => <NoTooltips>{content}</NoTooltips>, []);
  useFillMarginSlot(isCompactViewport ? hush : panel);

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
    (activeTab === "conversation" ? (conversationScrollport() ?? window) : window).scrollTo({
      top,
    });
  }, [activeTab]);

  // The page scrolls the window itself - the tree brings a focused row clear of the sticky
  // identity block (`identity-block.ts`'s `bringOnScreen`), and below the width the margin
  // keeps a column at the conversation places the reader at its newest card the same way - so
  // it puts the window back as it leaves. Nothing else resets it, and a page read through the
  // scroll the page before it left measures every box against the wrong viewport. A landing
  // the next page performs runs after this cleanup and stands.
  //
  // Back and Forward are the one exception: the browser restores the position it recorded for
  // the entry it is returning to, and zeroing the window would undo that. A pop in the same
  // task as the unmount is that reader, and the page leaves the window to them.
  const popped = useRef(0);
  useEffect(() => {
    const pop = () => {
      popped.current = Date.now();
    };
    window.addEventListener("popstate", pop);
    return () => {
      window.removeEventListener("popstate", pop);
      if (Date.now() - popped.current > POP_RESTORES_MS) window.scrollTo({ top: 0 });
    };
  }, []);

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
  // What the page draws beside the tree where the address names no document preset: the
  // issue's own address opens the spec there (`refs/routes.ts`'s `issueTabForRoute`). The
  // Decisions preset draws no document, and a node-menu address draws its own panel, which
  // carries that document's controls itself.
  const drawnBesideTree =
    activeTab === "spec" && (artifactRoute === undefined || isPrimaryArtifactRoute)
      ? primaryArtifact
      : undefined;
  // Whose toolbar the preset row draws: the document the preset has open, and on the Tree and
  // Decisions presets, which draw nodes of every document, the focused node's own (R16). A
  // focus on a node that belongs to no document falls back to the document the page is drawing
  // beside the tree, because that document is open in front of the reader and its version
  // picker, its name-a-version control and its approval are controls it has nowhere else
  // (R18, c-nothing-lost); a preset that draws no document at all leaves the row with no
  // toolbar.
  const openDocument =
    address.preset === "spec"
      ? primaryArtifact
      : address.preset === "checks"
        ? verificationArtifact
        : address.preset === "plan"
          ? planArtifact
          : (documentOfNode(focused.id, artifactsBySlug(issue.data))?.artifact ?? drawnBesideTree);
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
    // A flex column, so the identity block's fourth row can be drawn at its end on a phone
    // (`IssueHeader`'s `order-last`); every other child keeps its document order.
    //
    // `NoTooltips`: nothing this page draws keeps meaning for a pointer alone, because a
    // reader on a phone or on the keyboard never sees a tooltip (R2, R21). What a tooltip
    // said is drawn instead - the claim's instant beside its age, a link's whole address -
    // and the panel says the same for the margin's own cards (`IssueMargin.tsx`).
    <NoTooltips>
      <section className="flex min-w-0 flex-col">
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
              conversationList={conversationList}
              conversationState={conversationState}
              focused={focused}
              isClosed={isClosed}
              issue={issue.data}
              onConversationState={pickState}
            />
          </div>
        ) : null}
        <IssuePresetPanels
          activatedTabs={activatedTabs}
          activeTab={activeTab}
          artifactRoute={artifactRoute}
          askId={askId}
          commentId={commentId}
          diffs={diffs}
          editedDocument={editedDocument}
          highlightTerm={highlightTerm}
          isClosed={isClosed}
          isPrimaryArtifactRoute={isPrimaryArtifactRoute}
          issueKey={issueKey}
          onShowDiff={showDiff}
          onToolbarSink={toolbarSink}
          onVersionChange={selectDocumentVersion}
          planArtifact={planArtifact}
          planStage={planStage}
          primaryArtifact={primaryArtifact}
          route={route}
          selectedArtifact={selectedArtifact}
          selectedStage={selectedStage}
          specOpensItem={address.preset === "spec"}
          specStage={specStage}
          specVersion={specVersion}
          state={state.data}
          toolbars={toolbars}
          verificationArtifact={verificationArtifact}
          verificationStage={verificationStage}
        />
      </section>
    </NoTooltips>
  );
}
