import { type ReactNode, useCallback, useEffect, useRef, useState } from "react";

import type { Artifact } from "../../api/types";
import { textMutedOnCanvas } from "../../theme/classes";
import { useMediaQuery } from "../shell/useDialog";
import { useSetFocusedNode } from "./focused-node";
import { firstLineOf, type TreeNode } from "./issue-tree";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, nodeStateWord } from "./page-words";
import { CYCLE_STATES, EVERYTHING, TreeRows } from "./TreeRows";
import {
  NOTHING_OPENED,
  type OpenedBranches,
  pathTo,
  treeRows,
  zoomBreadcrumb,
  zoomedRows,
} from "./tree-fold";
import { everythingOpened, rowKeyOf } from "./tree-keys";
import { useNodeEditing } from "./useNodeEditing";
import { useTreeKeymap } from "./useTreeKeymap";

/**
 * The issue's body: one folded tree of its nodes (dispatch://INFRA-1196 R1, R2, R3, R6,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t14`).
 *
 * The model is `issue-tree.ts`'s, the preset's query over it is `presets.ts`'s and the fold is
 * `tree-fold.ts`'s, all three pure; this holds what only a reader's page can hold - what they
 * opened, what they pinned, and where the focus is. The rows themselves are drawn by
 * `TreeRows.tsx` and the caret in a node's own text is `useNodeEditing.ts`'s.
 *
 * The tree is the one body every preset draws: the page hands it the tree its preset left, and
 * a row drawn only because a match sits under it is context, faint and marked, and reached by
 * the arrow keys like any other row.
 *
 * The tree answers the WAI-ARIA tree pattern's roles and properties and keeps one tab stop: the
 * focused row is the only row that takes the focus. The keys that move it are
 * `dispatch://INFRA-1196/artifact/plan-md#b-t15`'s, so a reader uses the pointer until they land.
 *
 * At 1024 px and below the tree is zoomed, one node at a time: the path down to the node the
 * reader is on, that node, and its children as one-line rows, with no second indent level.
 */
export function IssueTree({
  artifacts,
  context,
  emptyLine,
  focusReveal,
  isClosed,
  onEditing,
  onReport,
  openBody,
  reveal,
  root,
}: {
  /** Each artifact of the issue by its slug, which is how a node names the document it is or
   *  belongs to (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The ids of the rows this preset draws only as the ancestor of a match. */
  context: ReadonlySet<string>;
  /** What the page says where the preset's query matched nothing under the issue. */
  emptyLine: string;
  /** Whether the node the address names is the tree's to put the focus on. It is not when the
   *  preset's own document is open on that node - a comment anchored in it, an ask indexed from
   *  one of its blocks, a block named by a fragment - because the document lands on it there,
   *  and two surfaces scrolling for the same node would each undo the other. */
  focusReveal: boolean;
  /** Whether the issue is closed, which the writes a node's body offers are refused on. */
  isClosed: boolean;
  /** The document the reader is editing a node of here, so the page draws that document's one
   *  editor rather than leaving the row with nothing to put a caret in; `undefined` whenever
   *  the reader is editing nothing. */
  onEditing: (artifactId: string | undefined) => void;
  /** What a key reported when it had nowhere to go, which the page draws in its own status
   *  at the top of the body (R9, R21): the tree raises it rather than drawing it, so the
   *  page's one report is the first status a reader of the page meets. */
  onReport: (report: string) => void;
  /** The node this preset draws the body of without being asked: what waits for the reader
   *  there, such as a decision waiting for their review (R7). It opens that body and takes no
   *  focus, which stays the reader's. */
  openBody?: string;
  /** The chain down to the node the address names, the issue first: the tree unfolds it and
   *  puts the focus on its last node, once per address (R19). */
  reveal: readonly TreeNode[] | undefined;
  /** The tree as the preset left it. */
  root: TreeNode;
}): ReactNode {
  const [opened, setOpened] = useState<OpenedBranches>(NOTHING_OPENED);
  const [focused, setFocused] = useState<string | undefined>(undefined);
  // Where the tree's one tab stop rests, which outlives the focus leaving it: Shift+Tab comes
  // back to the row the reader left, not to the first row (R9).
  const [stop, setStop] = useState<string | undefined>(undefined);
  const [pinned, setPinned] = useState<ReadonlySet<string>>(() => new Set());
  const [zoom, setZoom] = useState<string | undefined>(undefined);
  // How much of the whole tree the cycle key draws: its first lines, their bodies down to the
  // second level, then everything.
  const [cycle, setCycle] = useState(0);
  // What a key reports when it has nowhere to go is the page's own status, drawn at the top of
  // the body (`IssueBody`): the tree raises it and does not draw it.
  // The node a key is taking the reader to, once the row it asked for is on the page.
  const [wanted, setWanted] = useState<string | undefined>(undefined);
  const zoomed = useMediaQuery(ZOOMED_TREE_QUERY);
  const setFocusedNode = useSetFocusedNode();
  const body = useRef<HTMLDivElement | null>(null);
  const { edit, editing, stopEditing } = useNodeEditing({
    artifacts,
    container: body,
    onEditing,
    onReport,
  });

  const target = reveal?.at(-1);
  // The address's own node is unfolded to whatever the reader has opened by hand, at every
  // depth: a `#b-` fragment names a block of a document the page opens folded.
  const path = reveal ?? [];
  const shown: OpenedBranches =
    path.length === 0
      ? opened
      : {
          branches: new Set([...opened.branches, ...path.map((above) => above.id)]),
          deeper: new Set([...opened.deeper, ...path.map((above) => above.id)]),
        };
  // Where the zoomed tree is rooted. Nothing chosen opens it on the issue itself, so a reader
  // on a phone sees the whole issue and can reach any branch of it; an address that names a
  // node opens it on that node's parent, so the node the address named is one of the rows.
  const on = zoom ?? (target === undefined ? root.id : (reveal?.at(-2) ?? root).id);
  // A reader who zoomed into a node reads the tree from there: the node is the tree's root,
  // the path above it the breadcrumb, and everything below it folded as it was. At 1024 px and
  // below the tree is always zoomed, one node at a time, which is `zoomedRows`.
  const rooted =
    zoom === undefined || zoom === root.id ? root : (pathTo(root, zoom)?.at(-1) ?? root);
  const rows = zoomed
    ? zoomedRows(root, on, context)
    : treeRows(rooted, cycle === EVERYTHING ? everythingOpened(rooted) : shown);
  const breadcrumb: readonly TreeNode[] = zoomed
    ? zoomBreadcrumb(root, on)
    : zoom === undefined
      ? []
      : zoomBreadcrumb(root, zoom);

  const takeFocus = useCallback(
    (row: string, node: TreeNode) => {
      setFocused(row);
      setStop(row);
      setFocusedNode({
        ...(node.author === undefined ? {} : { author: node.author.id }),
        id: node.id,
        ...(node.state === undefined
          ? {}
          : { state: nodeStateWord(node.type, node.state) ?? node.state }),
        title: node.text === "" ? node.id : firstLineOf(node.text).line,
        type: node.type,
      });
    },
    [setFocusedNode]
  );
  // The focus leaving the tree closes the body it opened: nothing is open while the reader is
  // somewhere else, and the panel goes back to the issue's own facts (R2, R12). A row the
  // reader is editing in keeps its body: the document's editor moves into it, which drops the
  // focus for a frame as it leaves the page, and that is no reader leaving the tree.
  const releaseFocus = useCallback(
    (event: { currentTarget: HTMLElement; relatedTarget: EventTarget | null }) => {
      const to = event.relatedTarget;
      if (editing !== undefined) return;
      if (to instanceof Node && event.currentTarget.contains(to)) return;
      setFocused(undefined);
      setFocusedNode(undefined);
    },
    [editing, setFocusedNode]
  );
  const unfold = useCallback((id: string, open: boolean) => {
    setOpened((held) => {
      const branches = new Set(held.branches);
      if (open) branches.add(id);
      else branches.delete(id);
      return { branches, deeper: held.deeper };
    });
  }, []);
  const openDeeper = useCallback((id: string) => {
    setOpened((held) => ({ branches: held.branches, deeper: new Set(held.deeper).add(id) }));
  }, []);
  const pin = useCallback((row: string) => {
    setPinned((held) => {
      const next = new Set(held);
      if (!next.delete(row)) next.add(row);
      return next;
    });
  }, []);

  // Takes the reader to one node, wherever it is: the path down to it is unfolded, a zoom it
  // sits outside of is left, and the row takes the focus once this render has drawn it.
  const goToNode = useCallback(
    (id: string) => {
      const above = (pathTo(root, id) ?? []).slice(0, -1).map((node) => node.id);
      setOpened((held) => ({
        branches: new Set([...held.branches, ...above]),
        deeper: new Set([...held.deeper, ...above]),
      }));
      setZoom((held) =>
        held === undefined || held === id || above.includes(held) ? held : root.id
      );
      setWanted(id);
    },
    [root]
  );
  // Zooming to the issue is zooming out of everything, and it is held as the issue's own id
  // rather than as nothing chosen (`setZoom` below), so the zoomed tree stays on the whole
  // issue instead of falling back to where it opened.

  useEffect(() => {
    if (wanted === undefined) return;
    setWanted(undefined);
    body.current
      ?.querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(wanted)}"]`)
      ?.focus();
  }, [wanted]);

  useTreeKeymap({
    container: body,
    onCycle: () => setCycle((held) => (held + 1) % CYCLE_STATES),
    onEdit: edit,
    onEditEnd: stopEditing,
    onFold: unfold,
    onOpenDeeper: openDeeper,
    onPin: pin,
    onReport,
    onReveal: goToNode,
    onZoom: setZoom,
    oneNodeAtATime: zoomed,
    rows,
    root,
    zoom: zoomed ? on : zoom,
  });

  // An address that names a node puts the focus on it (R19). The row is not on the page until
  // the read that holds it has answered, and the document the preset draws beside the tree
  // mounts later still and can leave the focus nowhere as it does, so this looks again on
  // every tree the page draws and takes the focus whenever nothing else holds it. Once the
  // reader - or anything else on the page - has the focus, it is theirs: the effect leaves it
  // alone until another address names another node.
  const taken = useRef<string | undefined>(undefined);
  const targetId = target?.id;
  useEffect(() => {
    if (targetId === undefined || !focusReveal) return;
    const loose = document.activeElement === null || document.activeElement === document.body;
    if (taken.current === targetId && !loose) return;
    if (!rows.some((row) => row.kind === "node" && row.node.id === targetId)) return;
    const row = body.current?.querySelector<HTMLElement>(
      `[data-node-row][data-node-id="${CSS.escape(targetId)}"]`
    );
    if (row === null || row === undefined) return;
    taken.current = targetId;
    row.focus();
  }, [focusReveal, rows, targetId]);

  // The tree is one tab stop, and it rests where the reader last stood: before they have
  // touched it that is its first row, so Tab reaches the tree rather than passing it by, and
  // after they have left it that is the row they left, so Shift+Tab brings them back to it.
  const keys = rows.map(rowKeyOf);
  const first = rows[0];
  const tabStop =
    stop !== undefined && keys.includes(stop)
      ? stop
      : first === undefined
        ? undefined
        : rowKeyOf(first);
  return (
    <div className="mb-3 flex min-w-0 flex-col">
      {breadcrumb.length === 0 ? null : (
        <p className={`flex min-w-0 flex-wrap items-baseline gap-1 text-xs ${textMutedOnCanvas}`}>
          {breadcrumb.map((above) => (
            <button
              // A crumb is a word in a line of text, not a control box: below 1280 px a plain
              // button is 44 px tall (`styles.css`), which is a third of the head the first
              // open question needs on a phone (R6). Its hit box grows by padding instead.
              className="dispatch-inline-link max-w-full truncate underline"
              key={above.id}
              onClick={() => setZoom(above.id)}
              tabIndex={-1}
              type="button"
              {...authorText}
            >
              {above.text === "" ? above.id : firstLineOf(above.text).line}
            </button>
          ))}
          <span {...pageChrome}>/</span>
        </p>
      )}
      <div
        aria-label={CONTROL_LABELS.tree}
        className="flex min-w-0 flex-col"
        onBlur={releaseFocus}
        ref={body}
        role="tree"
      >
        <TreeRows
          artifacts={artifacts}
          context={context}
          cycle={cycle}
          editing={editing}
          focused={focused}
          isClosed={isClosed}
          onFocus={takeFocus}
          onOpenDeeper={openDeeper}
          onPin={pin}
          onToggle={(node, expanded) =>
            zoomed ? setZoom(node.id) : unfold(node.id, expanded !== true)
          }
          {...(openBody === undefined ? {} : { openBody })}
          pinned={pinned}
          rows={rows}
          tabStop={tabStop}
          zoomed={zoomed}
        />
      </div>
      {root.children.length === 0 ? (
        <p className={`py-1 text-sm ${textMutedOnCanvas}`} {...pageChrome}>
          {emptyLine}
        </p>
      ) : null}
    </div>
  );
}

/** The width the tree is read one node at a time at, where five levels of indent and a row's own
 *  words do not fit together (R1, R17). */
const ZOOMED_TREE_QUERY = "(max-width: 1024px)";
