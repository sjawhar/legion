import type { ReactNode } from "react";

import type { Artifact, UserState } from "../../api/types";
import { ActivityTab } from "../activity/ActivityTab";
import { ArtifactDocument } from "../artifacts/ArtifactDocument";
import { ArtifactRoutePanel } from "../artifacts/ArtifactRoutePanel";
import type { DocumentToolbar } from "../doc/ProofDocument";
import { PlanTab } from "../plan/PlanTab";
import type { IssueRoute, IssueTab } from "../refs/routes";
import { DocumentStage } from "./document-stage";
import { RoleDocumentPanel } from "./RoleDocumentPanel";
import { SpecRequirements } from "./SpecRequirements";

/** Where a document's editor is mounted so a preset can take it onto its own stage
 *  (`document-stage.tsx`'s `useDocumentStage`). */
type DocumentStageRef = (node: HTMLElement | null) => void;

/**
 * What each preset draws beside the tree (dispatch://INFRA-1196 R16): the spec, the
 * Verification and Plan documents, whatever an `/artifacts/<slug>` address opens, the one
 * editor a row of the tree is being edited in, and the Activity list.
 *
 * These are plain panels, not tabpanels: the presets are queries over one tree and the page has
 * no tablist of panels any more. A panel the reader has never opened stays unmounted, and one
 * they have opened keeps rendering while hidden, which is what keeps the spec's editor
 * connected across a preset change (`activated`).
 *
 * It is a component of its own because the page's column is the identity block, the body and
 * this: the first two decide the page's shape, and this decides nothing - it draws the document
 * each address names, out of values `IssuePage` already holds.
 */
export function IssuePresetPanels({
  activatedTabs,
  activeTab,
  artifactRoute,
  askId,
  commentId,
  diffs,
  editedDocument,
  highlightTerm,
  isClosed,
  isPrimaryArtifactRoute,
  issueKey,
  onShowDiff,
  onToolbarSink,
  onVersionChange,
  planArtifact,
  planStage,
  primaryArtifact,
  route,
  selectedArtifact,
  selectedStage,
  specOpensItem,
  specStage,
  specVersion,
  state,
  toolbars,
  verificationArtifact,
  verificationStage,
}: {
  activatedTabs: Record<IssueTab, boolean>;
  activeTab: IssueTab;
  artifactRoute: Extract<IssueRoute, { kind: "artifact" }> | undefined;
  askId: string | undefined;
  commentId: string | undefined;
  /** Which documents are drawn against their previous version. */
  diffs: ReadonlySet<string>;
  /** The document a row of the tree is being edited in, where no panel here draws it. */
  editedDocument: Artifact | undefined;
  highlightTerm: string | undefined;
  isClosed: boolean;
  isPrimaryArtifactRoute: boolean;
  issueKey: string;
  onShowDiff: (artifactId: string, next: boolean) => void;
  onToolbarSink: (artifactId: string) => (next: DocumentToolbar | undefined) => void;
  onVersionChange: (artifact: Artifact, version: number | null) => void;
  planArtifact: Artifact | undefined;
  planStage: DocumentStageRef;
  primaryArtifact: Artifact;
  route: IssueRoute;
  selectedArtifact: Artifact | undefined;
  selectedStage: DocumentStageRef;
  /** Whether the `?ask=`/`?comment=` an address carries is the spec's to open: only where the
   *  Spec preset is what the address opens, since on every other preset the tree is what the
   *  address names and an editor focusing the item would take the focus off its row (R19). */
  specOpensItem: boolean;
  specStage: DocumentStageRef;
  specVersion: number | undefined;
  state: UserState | undefined;
  toolbars: ReadonlyMap<string, DocumentToolbar>;
  verificationArtifact: Artifact | undefined;
  verificationStage: DocumentStageRef;
}): ReactNode {
  return (
    <>
      <div aria-hidden={activeTab !== "spec"} hidden={activeTab !== "spec"} id="issue-spec-panel">
        {activatedTabs.spec && !(artifactRoute !== undefined && !isPrimaryArtifactRoute) ? (
          <div className="space-y-4">
            <div className="flex min-w-0 flex-col" ref={specStage} />
            <DocumentStage artifactId={primaryArtifact.id}>
              <ArtifactDocument
                artifact={primaryArtifact}
                askId={specOpensItem ? askId : undefined}
                commentId={specOpensItem ? commentId : undefined}
                highlightTerm={highlightTerm}
                isClosed={isClosed}
                onToolbarChange={onToolbarSink(primaryArtifact.id)}
                owner={{ key: issueKey, kind: "issue" }}
                onVersionChange={(version) => {
                  onShowDiff(primaryArtifact.id, false);
                  onVersionChange(primaryArtifact, version);
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
        activated={activatedTabs.verification}
        active={activeTab === "verification"}
        artifact={verificationArtifact}
        isClosed={isClosed}
        issueKey={issueKey}
        onToolbarChange={onToolbarSink}
        onVersionChange={onVersionChange}
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
        onToolbarChange={onToolbarSink}
        onVersionChange={onVersionChange}
        showDiff={diffs}
        stage={planStage}
        tab="plan"
      />
      <ArtifactRoutePanel
        active={activeTab === "artifacts"}
        artifact={selectedArtifact}
        artifactRoute={artifactRoute}
        isClosed={isClosed}
        isPrimaryArtifactRoute={isPrimaryArtifactRoute}
        mounted={activatedTabs.artifacts}
        onShowDiffChange={(next) => onShowDiff(selectedArtifact?.id ?? "", next)}
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
                onToolbarChange={onToolbarSink(selectedArtifact.id)}
                owner={{ key: issueKey, kind: "issue" }}
                onVersionChange={(version) => {
                  onShowDiff(selectedArtifact.id, false);
                  onVersionChange(selectedArtifact, version);
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
            onToolbarChange={onToolbarSink(editedDocument.id)}
            owner={{ key: issueKey, kind: "issue" }}
            onVersionChange={(version) => onVersionChange(editedDocument, version)}
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
        {activatedTabs.activity ? <ActivityTab issueKey={issueKey} state={state} /> : null}
      </div>
    </>
  );
}
