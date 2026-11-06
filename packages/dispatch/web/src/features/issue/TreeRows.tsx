import type { ReactNode } from "react";

import type { Artifact } from "../../api/types";
import { borderDefault, borderTransparent, textMutedOnCanvas } from "../../theme/classes";
import { documentOfNode } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { NodeRow } from "./NodeRow";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import type { DrawnRow } from "./tree-fold";
import { rowKeyOf } from "./tree-keys";

/**
 * The rows of the issue's tree, drawn (dispatch://INFRA-1196 R1, R2).
 *
 * It is the drawing alone: which rows there are is the fold's (`tree-fold.ts`), and what the
 * reader opened, pinned, focused and is editing in is `IssueTree.tsx`'s, which hands each row
 * what it is. A row below the levels the fold drew is one line saying how many nodes it holds
 * and opening them in place.
 */
export function TreeRows({
  artifacts,
  context,
  cycle,
  editing,
  focused,
  isClosed,
  onFocus,
  onOpenDeeper,
  onPin,
  onToggle,
  openBody,
  pinned,
  rows,
  tabStop,
  zoomed,
}: {
  /** Each artifact of the issue by its slug (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The ids of the rows this preset draws only as the ancestor of a match. */
  context: ReadonlySet<string>;
  /** Which of the cycle key's three states the tree is in (`CYCLE_STATES`). */
  cycle: number;
  /** The row the reader is editing in and the document its editor belongs to. */
  editing: { readonly artifactId: string; readonly row: string } | undefined;
  /** The key of the row that holds the focus. */
  focused: string | undefined;
  isClosed: boolean;
  onFocus: (row: string, node: TreeNode) => void;
  onOpenDeeper: (id: string) => void;
  onPin: (row: string) => void;
  onToggle: (node: TreeNode, expanded: boolean | undefined) => void;
  /** The node whose body the preset opens without being asked (R7). */
  openBody?: string;
  /** The keys of the rows whose bodies the reader pinned open. */
  pinned: ReadonlySet<string>;
  rows: readonly DrawnRow[];
  /** The key of the row that is the tree's one tab stop. */
  tabStop: string | undefined;
  /** Whether the tree is read one node at a time (`ZOOMED_TREE_QUERY`). */
  zoomed: boolean;
}): ReactNode {
  return (
    <>
      {rows.map((row) => {
        const key = rowKeyOf(row);
        const indent = (row.level - 1) * INDENT_PER_LEVEL;
        if (row.kind === "deeper") {
          return (
            <BranchMore
              held={row.held}
              indent={indent}
              key={key}
              level={row.level}
              onOpen={() => onOpenDeeper(row.of.id)}
            />
          );
        }
        const place = documentOfNode(row.node.id, artifacts);
        return (
          <NodeRow
            {...(place?.blockId === undefined && place !== undefined
              ? { artifact: place.artifact }
              : {})}
            body={
              focused === key ||
              pinned.has(key) ||
              editing?.row === key ||
              row.node.id === openBody ||
              drawsBody(cycle, row.level)
            }
            context={context.has(row.node.id)}
            {...(place?.blockId === undefined ? {} : { document: place.artifact })}
            {...(editing?.row === key ? { editing: editing.artifactId } : {})}
            expanded={row.expanded}
            focused={focused === key}
            folded={row.folded}
            indent={indent}
            isClosed={isClosed}
            key={key}
            level={row.level}
            node={row.node}
            onFocus={() => onFocus(key, row.node)}
            onPin={() => onPin(key)}
            onToggle={() => onToggle(row.node, row.expanded)}
            oneLine={zoomed}
            pinned={pinned.has(key)}
            tabStop={tabStop === key}
          />
        );
      })}
    </>
  );
}

/** How far in one level of the tree is drawn, in pixels (R1). */
const INDENT_PER_LEVEL = 20;

/**
 * How much of the whole tree the cycle key draws, in the three states it steps through: its
 * rows as first lines, those first lines with every body down to the second level open, and
 * everything - every branch unfolded and every body open (R9). The reader's own folds and pins
 * are untouched by it, so the cycle comes back to the tree they had.
 */
export const CYCLE_STATES = 3;

/** The state that unfolds every branch, which is the last of the three. */
export const EVERYTHING = CYCLE_STATES - 1;

/** How deep the second state of the cycle opens the bodies. */
const BODIES_TO_LEVEL = 2;

/** Whether the cycle draws the body of a row at `level`, whatever the focus and the pins say. */
function drawsBody(cycle: number, level: number): boolean {
  return cycle === EVERYTHING || (cycle === 1 && level <= BODIES_TO_LEVEL);
}

/** The one row a branch below the drawn levels folds to: how many nodes it holds, and the
 *  control that opens them in place. */
function BranchMore({
  held,
  indent,
  level,
  onOpen,
}: {
  held: number;
  indent: number;
  level: number;
  onOpen: () => void;
}): ReactNode {
  return (
    <div
      aria-level={level}
      className={`flex min-w-0 border-l-2 py-1 text-xs ${borderTransparent} ${textMutedOnCanvas}`}
      data-branch-more=""
      role="treeitem"
      style={{ paddingLeft: indent }}
      tabIndex={-1}
    >
      <button
        className={`rounded border px-1 ${borderDefault}`}
        onClick={onOpen}
        tabIndex={-1}
        type="button"
        {...pageChrome}
      >
        <span {...authorText}>{held}</span> {CONTROL_LABELS.deeper_nodes}
      </button>
    </div>
  );
}
