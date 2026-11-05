import { type ReactNode, useMemo } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { primarySpec } from "../../api/issue-cache";
import type { Artifact, IssueDetails } from "../../api/types";
import { IssueChildren } from "./IssueChildren";
import { IssuePresets } from "./IssuePresets";
import { IssueThreeThings, nodesOfType, specSummary } from "./IssueThreeThings";
import { IssueTree } from "./IssueTree";
import type { AddressTarget, IssueAddress } from "./issue-address";
import { pathToTarget } from "./issue-address";
import { EMPTY_STATE_LINES, PRESET_ORDER } from "./page-words";
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
  issue,
  onLeave,
  plan,
  toolbar,
}: {
  address: IssueAddress;
  issue: IssueDetails;
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
  const named = useMemo(() => {
    const node = reveal?.at(-1);
    return new Set(node === undefined ? [] : [node.id]);
  }, [reveal]);
  const view = useMemo(
    () => presetView(root, address.preset, address.filter, named),
    [address.filter, address.preset, named, root]
  );
  const specSlug = primarySpec(issue)?.slug ?? "";

  return (
    <>
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
        toolbar={toolbar}
      />
      <IssueChildren issue={issue} />
      <IssueTree
        context={view.context}
        emptyLine={EMPTY_STATE_LINES[address.preset]}
        focusReveal={address.preset !== "spec" && targetKind !== "node"}
        reveal={reveal}
        root={view.root}
      />
    </>
  );
}
