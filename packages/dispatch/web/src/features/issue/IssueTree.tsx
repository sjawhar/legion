import { type ReactNode, useCallback, useEffect, useRef, useState } from "react";

import type { Artifact } from "../../api/types";
import { borderDefault, borderTransparent, textMutedOnCanvas } from "../../theme/classes";
import { useMargin } from "../margin/margin-context";
import { useMediaQuery } from "../shell/useDialog";
import { useSetFocusedNode } from "./focused-node";
import { documentOfNode } from "./issue-artifacts";
import { firstLineOf, type TreeNode } from "./issue-tree";
import { NodeRow } from "./NodeRow";
import { caretInBlock } from "./node-caret";
import { authorText, KEY_REPORTS, pageChrome } from "./page-structure";
import { CONTROL_LABELS, nodeStateWord } from "./page-words";
import {
  NOTHING_OPENED,
  type OpenedBranches,
  pathTo,
  treeRows,
  zoomBreadcrumb,
  zoomedRows,
  zoomOpensOn,
} from "./tree-fold";
import { everythingOpened, rowKeyOf } from "./tree-keys";
import { useTreeKeymap } from "./useTreeKeymap";

/**
 * The issue's body: one folded tree of its nodes (dispatch://INFRA-1196 R1, R2, R3, R6,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t14`).
 *
 * The model is `issue-tree.ts`'s, the preset's query over it is `presets.ts`'s and the fold is
 * `tree-fold.ts`'s, all three pure; this holds what only a reader's page can hold - what they
 * opened, what they pinned, and where the focus is - and draws the rows (`NodeRow.tsx`).
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
  onEditing,
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
  /** The document the reader is editing a node of here, so the page draws that document's one
   *  editor rather than leaving the row with nothing to put a caret in; `undefined` whenever
   *  the reader is editing nothing. */
  onEditing: (artifactId: string | undefined) => void;
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
  // What a key reported when it had nowhere to go, in the page's own status.
  const [report, setReport] = useState("");
  // The node a key is taking the reader to, once the row it asked for is on the page.
  const [wanted, setWanted] = useState<string | undefined>(undefined);
  // The row the reader is editing in, with the document its node's text lives in: that
  // document's one editor is drawn in this row's body while it stands (R9).
  const [editing, setEditing] = useState<EditedRow | undefined>(undefined);
  const zoomed = useMediaQuery(ZOOMED_TREE_QUERY);
  const setFocusedNode = useSetFocusedNode();
  const { documentBridge } = useMargin();
  const body = useRef<HTMLDivElement | null>(null);
  // The node the reader has just left the caret in, so the row takes the focus back.
  const leaving = useRef<string | undefined>(undefined);

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
  const on = zoom ?? (target === undefined ? zoomOpensOn(root) : (reveal?.at(-2) ?? root).id);
  // A reader who zoomed into a node reads the tree from there: the node is the tree's root,
  // the path above it the breadcrumb, and everything below it folded as it was. At 1024 px and
  // below the tree is always zoomed, one node at a time, which is `zoomedRows`.
  const rooted = zoom === undefined ? root : (pathTo(root, zoom)?.at(-1) ?? root);
  const rows = zoomed
    ? zoomedRows(root, on)
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
        held === undefined || held === id || above.includes(held) ? held : undefined
      );
      setWanted(id);
    },
    [root]
  );
  // The issue itself is the whole tree, so zooming to it is zooming out of everything.
  const zoomTo = useCallback(
    (id: string | undefined) => setZoom(id === root.id ? undefined : id),
    [root.id]
  );
  // The caret goes into the node's own text where that text lives, which is its document's
  // block, with the collaborative cursors and the comment chord it has there (R9, the Design
  // section's "Editing"). The document's one editor is moved onto this row
  // (`document-stage.tsx`) and the caret is placed in the node's own block of it
  // (`node-caret.ts`); a node whose text no document of this issue holds has none to put a
  // caret in, and the key says so.
  const edit = useCallback(
    (row: string, node: TreeNode) => {
      const place = documentOfNode(node.id, artifacts);
      if (place === undefined) {
        setReport(KEY_REPORTS.no_text);
        return;
      }
      setEditing({
        artifactId: place.artifact.id,
        ...(place.blockId === undefined ? {} : { blockId: place.blockId }),
        node: node.id,
        row,
      });
    },
    [artifacts]
  );
  const stopEditing = useCallback(() => {
    setEditing((held) => {
      if (held !== undefined) leaving.current = held.node;
      return undefined;
    });
  }, []);

  useEffect(() => {
    if (wanted === undefined) return;
    setWanted(undefined);
    body.current
      ?.querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(wanted)}"]`)
      ?.focus();
  }, [wanted]);

  // The page is told which document a row is editing, so it draws that document's one editor,
  // and the caret goes in once the stage has moved into the row - one frame later, because the
  // page mounts the editor in the commit this effect runs after.
  const editingArtifact = editing?.artifactId;
  const editingBlock = editing?.blockId;
  const editingNode = editing?.node;
  useEffect(() => {
    onEditing(editingArtifact);
  }, [editingArtifact, onEditing]);
  useEffect(() => {
    if (editingNode === undefined) return;
    const frame = requestAnimationFrame(() => {
      const host = body.current?.querySelector<HTMLElement>(
        `[data-node-row][data-node-id="${CSS.escape(editingNode)}"] [data-node-body]`
      );
      if (host === null || host === undefined) return;
      caretInBlock(host, editingBlock);
      // The block is scrolled into view and pulsed by the document's own editor, where that
      // editor is the one the margin holds; the caret above is the row's doing either way.
      const bridge = documentBridge;
      if (
        editingBlock !== undefined &&
        bridge !== undefined &&
        bridge.artifactId === editingArtifact
      ) {
        bridge.focusBlock(editingBlock);
      }
    });
    return () => cancelAnimationFrame(frame);
  }, [documentBridge, editingArtifact, editingBlock, editingNode]);

  // Escape leaves the caret for the row it came from. The editor the row holds calls
  // `preventDefault` on the key before it reaches the page's keyboard registry, which drops
  // every binding of the press (`features/shell/keymap.ts`'s `handleKeyDown`), so the row
  // listens for it in the capture phase while it is being edited in - before the editor sees
  // it - and the registry's own binding stands for every other surface
  // (`useTreeKeymap`'s `tree-leave-caret`).
  useEffect(() => {
    const container = body.current;
    if (editingNode === undefined || container === null) return;
    const leave = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      event.preventDefault();
      (document.activeElement as HTMLElement | null)?.blur();
      container
        .querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(editingNode)}"]`)
        ?.focus();
      stopEditing();
    };
    container.addEventListener("keydown", leave, true);
    return () => container.removeEventListener("keydown", leave, true);
  }, [editingNode, stopEditing]);

  // Leaving the caret leaves the row with the focus. The editor asserts its own selection
  // again a moment after a blur (`prosemirror-view`'s focus handler), and its stage moves off
  // this row in the same commit, so the row takes the focus once more on the next frame as
  // well as the moment Escape was pressed (`useTreeKeymap`'s `tree-leave-caret`).
  useEffect(() => {
    const left = leaving.current;
    if (editingNode !== undefined || left === undefined) return;
    leaving.current = undefined;
    const frame = requestAnimationFrame(() => {
      body.current
        ?.querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(left)}"]`)
        ?.focus();
    });
    return () => cancelAnimationFrame(frame);
  }, [editingNode]);

  useTreeKeymap({
    container: body,
    onCycle: () => setCycle((held) => (held + 1) % CYCLE_STATES),
    onEdit: edit,
    onEditEnd: stopEditing,
    onFold: unfold,
    onOpenDeeper: openDeeper,
    onPin: pin,
    onReport: setReport,
    onReveal: goToNode,
    onZoom: zoomTo,
    oneNodeAtATime: zoomed,
    rows,
    root,
    zoom,
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
      {/* What a key reported when it had nowhere to go: the page's own status, which is empty
          while every key the reader pressed went somewhere (R9). */}
      <p className={`text-xs ${textMutedOnCanvas}`} role="status" {...pageChrome}>
        {report}
      </p>
      {breadcrumb.length === 0 ? null : (
        <p className={`flex min-w-0 flex-wrap items-baseline gap-1 text-xs ${textMutedOnCanvas}`}>
          {breadcrumb.map((above) => (
            <button
              className="max-w-full truncate underline"
              key={above.id}
              onClick={() => zoomTo(above.id)}
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
                onOpen={() => openDeeper(row.of.id)}
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
                drawsBody(cycle, row.level)
              }
              context={context.has(row.node.id)}
              {...(editing?.row === key ? { editing: editing.artifactId } : {})}
              expanded={row.expanded}
              focused={focused === key}
              folded={row.folded}
              indent={indent}
              key={key}
              level={row.level}
              node={row.node}
              onFocus={() => takeFocus(key, row.node)}
              onPin={() => pin(key)}
              onToggle={() =>
                zoomed ? zoomTo(row.node.id) : unfold(row.node.id, row.expanded !== true)
              }
              oneLine={zoomed}
              pinned={pinned.has(key)}
              tabStop={tabStop === key}
            />
          );
        })}
      </div>
      {root.children.length === 0 ? (
        <p className={`py-1 text-sm ${textMutedOnCanvas}`} {...pageChrome}>
          {emptyLine}
        </p>
      ) : null}
    </div>
  );
}

/** One row the reader is editing in: the document its node's text lives in, the block of that
 *  document the node is, and which row of which node is holding the editor. */
interface EditedRow {
  readonly artifactId: string;
  readonly blockId?: string;
  readonly node: string;
  readonly row: string;
}

/** The width the tree is read one node at a time at, where five levels of indent and a row's own
 *  words do not fit together (R1, R17). */
const ZOOMED_TREE_QUERY = "(max-width: 1024px)";

/** How far in one level of the tree is drawn, in pixels (R1). */
const INDENT_PER_LEVEL = 20;

/**
 * How much of the whole tree the cycle key draws, in the three states it steps through: its
 * rows as first lines, those first lines with every body down to the second level open, and
 * everything - every branch unfolded and every body open (R9). The reader's own folds and pins
 * are untouched by it, so the cycle comes back to the tree they had.
 */
const CYCLE_STATES = 3;

/** The state that unfolds every branch, which is the last of the three. */
const EVERYTHING = CYCLE_STATES - 1;

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
