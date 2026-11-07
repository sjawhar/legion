import { type ReactNode, useMemo, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { primarySpec } from "../../api/issue-cache";
import type { Artifact, IssueDetails } from "../../api/types";
import { textMutedOnCanvas } from "../../theme/classes";
import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { IssueChildren } from "./IssueChildren";
import { IssuePresets } from "./IssuePresets";
import { IssueThreeThings, nodesOfType, specSummary } from "./IssueThreeThings";
import { IssueTree } from "./IssueTree";
import type { AddressTarget, IssueAddress } from "./issue-address";
import { pathToTarget } from "./issue-address";
import { artifactsBySlug, uncitedArtifacts } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { pageChrome } from "./page-structure";
import { EMPTY_STATE_LINES, PRESET_ORDER, type Preset, WAITING_LINES } from "./page-words";
import { presetCounts, presetView } from "./presets";
import { useIssueTree } from "./useIssueTree";

/**
 * The issue's body on every address: its three sentences, the preset row, the children section
 * and the one tree the preset is a query over (dispatch://INFRA-1196 R1, R3, R5, R10).
 *
 * It is one component because all four read the same tree: the head reads the spec's summary
 * node and the issue's checks, the row counts what each preset's query holds, and the tree
 * draws what the open preset left. Building that tree is this page's one model read
 * (`useIssueTree.ts`), so it happens here, once, below the guards `IssuePage` draws for an
 * issue it could not load.
 */
export function IssueBody({
  address,
  isClosed,
  issue,
  onEditing,
  onLeave,
  plan,
  toolbar,
}: {
  address: IssueAddress;
  /** Whether the issue is closed, which the node menu's upload reads. */
  isClosed: boolean;
  issue: IssueDetails;
  /** The document a row of the tree is being edited in, so the page draws that document's one
   *  editor for the row to take; `undefined` while the reader is editing nothing. */
  onEditing: (artifactId: string | undefined) => void;
  /** Called before a preset's own control leaves this address, so the page can record where
   *  the reader was before the move. */
  onLeave: () => void;
  /** The issue's plan document, whose state the head's first row draws. */
  plan: Artifact | undefined;
  /** The open document's toolbar, where the preset has a document of its own. */
  toolbar?: ReactNode;
}): ReactNode {
  const navigate = useNavigate();
  const { hash, pathname, search } = useLocation();
  const root = useIssueTree(issue);
  const counts = useMemo(
    () => presetCounts(root, address.filter, PRESET_ORDER),
    [address.filter, root]
  );
  // The address's own node is resolved against the whole tree and then kept by the preset's
  // query, so an address that names a node the preset would not have matched still draws it.
  // The target is read by what it says rather than by the value `issueAddress` built, which is
  // a new one on every render: the tree is walked and pruned again only when the address or
  // the tree itself changes, not on every commit the page makes.
  const targetKind = address.target?.kind;
  const targetId = address.target?.id;
  const target = useMemo<AddressTarget | undefined>(
    () =>
      targetKind === undefined || targetId === undefined
        ? undefined
        : // The two fields are the whole of a target, and this is the one place that says so,
          // so a target holds its value across a render rather than being built afresh.
          ({ id: targetId, kind: targetKind } as AddressTarget),
    [targetId, targetKind]
  );
  const reveal = useMemo(() => pathToTarget(root, target), [root, target]);
  // The node the preset draws the body of without being asked: the decision waiting for the
  // reader's review, whose body holds Keep and Change, so a reader who opens the preset is in
  // front of what waits for them (R7). It opens the body and never takes the focus, which is
  // the reader's.
  const opened = useMemo(
    () => (target === undefined ? opensOn(root, address.preset) : undefined),
    [address.preset, root, target]
  );
  const named = useMemo(() => {
    const node = reveal?.at(-1);
    return new Set(node === undefined ? [] : [node.id]);
  }, [reveal]);
  const view = useMemo(
    () => presetView(root, address.preset, address.filter, named),
    [address.filter, address.preset, named, root]
  );
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  const uncited = useMemo(() => uncitedArtifacts(root, issue.artifacts), [issue.artifacts, root]);
  const specSlug = primarySpec(issue)?.slug ?? "";
  // What a key reported when it had nowhere to go, raised by the tree (`IssueTree`'s
  // `onReport`) and drawn here as the page's own status.
  const [report, setReport] = useState("");
  // Where the open document's toolbar goes. From `md` it sits beside the preset row, as the
  // design draws it (R16). Below `md` it is 96 px of a 844 px screen - the version picker, the
  // name-a-version control and the approval, two lines of 44 px controls - and the identity
  // block, the head, the preset row and the children section already take 700 px of it, so it
  // is drawn under the tree instead, where the document it belongs to is: the same move the
  // identity block's fourth row makes at that width (`IssueHeader.tsx`), and for the same
  // reason - the first open question has to be on a phone's first screen (R6). Nothing is
  // behind a control either way, so every one of its controls is still drawn with nothing
  // opened (R18).
  const isPhoneViewport = useMediaQuery(PHONE_VIEWPORT_QUERY);

  return (
    <>
      {/* What a key reported when it had nowhere to go: the page's one `role="status"`, which
          is empty while every key the reader pressed went somewhere (R9, R21). It is drawn
          here, above everything, so the page's own report is the first status a reader meets -
          ahead of the live-connection status a document's toolbar carries. */}
      <p className={`text-xs ${textMutedOnCanvas}`} role="status" {...pageChrome}>
        {report}
      </p>
      <IssueThreeThings
        checks={nodesOfType(root, "check")}
        issueKey={issue.key}
        plan={plan}
        planTasks={nodesOfType(root, "plan_task").length}
        spec={primarySpec(issue)}
        summary={specSummary(root, specSlug)}
      />
      <IssuePresets
        active={address.preset}
        counts={counts}
        filter={address.filter}
        isClosed={isClosed}
        issueKey={issue.key}
        nodeMenu={address.nodeMenu}
        onLeave={onLeave}
        onFilterChange={(next) => {
          const query = new URLSearchParams(search);
          if (next === "") query.delete("q");
          else query.set("q", next);
          const rest = query.toString();
          navigate(`${pathname}${rest === "" ? "" : `?${rest}`}${hash}`, { replace: true });
        }}
        toolbar={isPhoneViewport ? undefined : toolbar}
        uncited={uncited}
      />
      <IssueChildren issue={issue} />
      {/* What the Checks preset does not hold yet: the coverage read counts the checks that
          serve each requirement and names none of them, and the chain read that would is not
          served (R20). The counts themselves are on each requirement's own body. */}
      {address.preset === "checks" ? (
        <p className={`mb-1 text-xs ${textMutedOnCanvas}`} {...pageChrome}>
          {WAITING_LINES.chain}
        </p>
      ) : null}
      <IssueTree
        artifacts={artifacts}
        context={view.context}
        emptyLine={EMPTY_STATE_LINES[address.preset]}
        focusReveal={address.preset !== "spec" && targetKind !== "node" && targetKind !== "turn"}
        isClosed={isClosed}
        onEditing={onEditing}
        onReport={setReport}
        openBody={opened}
        reveal={reveal}
        root={view.root}
      />
      {isPhoneViewport && toolbar !== undefined ? (
        <div className="mt-1 flex min-w-0 flex-wrap items-center gap-2">{toolbar}</div>
      ) : null}
    </>
  );
}

/**
 * The chain down to the node a preset opens on when the address names none: on the Decisions
 * preset the first decision waiting for the reader's review, so the reader lands on what waits
 * for them with Keep and Change in its body (R6, R7). Every other preset opens on no node.
 */
function opensOn(root: TreeNode, preset: Preset): string | undefined {
  if (preset !== "decisions") return undefined;
  return nodesOfType(root, "decision").find((node) => node.waiting)?.id;
}
