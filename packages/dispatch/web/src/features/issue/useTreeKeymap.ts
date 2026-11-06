import { type RefObject, useRef } from "react";

import { useKeymap } from "../shell/keymap";
import type { TreeNode } from "./issue-tree";
import { KEY_REPORTS } from "./page-structure";
import { type DrawnRow, pathTo } from "./tree-fold";
import { decisionBehind, nextQuestion, rowKeyOf } from "./tree-keys";

/**
 * The tree's keyboard: every row of the key table in the Design section of
 * dispatch://INFRA-1196's spec that acts on the tree itself
 * (`dispatch://INFRA-1196/artifact/plan-md#b-t15`). The chords that open a preset, the panel,
 * the children or the node menu are the page's (`useIssueKeymap.ts`), because each of those is
 * an address rather than a move inside the tree.
 *
 * Every binding is registered in the page's own `issue` scope, so the registry suspends all of
 * them while a caret is in a node's text: reading mode is where no caret is, which the keymap
 * already enforces inside an editable and this module does not restate. Escape is the one key
 * that fires there, because leaving the caret is what it is for.
 *
 * The keys move the focus by focusing a row, never by writing the tree's own state behind the
 * browser's back: a row that takes the focus tells the tree (`NodeRow`'s `onFocus`), which
 * opens its body, scrolls it clear of the sticky identity block and leaves the tab stop on it.
 * So a key does exactly what a reader's pointer does, and the tree has one answer to where the
 * reader is.
 */
export interface TreeKeyboard {
  /** The element the rows are drawn in, which is the tree itself. */
  readonly container: RefObject<HTMLElement | null>;
  /** Steps the cycle that says how much of the whole tree is drawn. */
  readonly onCycle: () => void;
  /** Puts the caret in one node's own text, in the row the key was pressed on. */
  readonly onEdit: (row: string, node: TreeNode) => void;
  /** Says the caret has left a node's text, so the row gives the document back. */
  readonly onEditEnd: () => void;
  /** Opens or closes one branch. */
  readonly onFold: (id: string, open: boolean) => void;
  /** Draws a branch that sits below the drawn levels where it stands. */
  readonly onOpenDeeper: (id: string) => void;
  /** Keeps one row's body open, or lets it close. */
  readonly onPin: (row: string) => void;
  /** Says where a key could not go, in the page's own status, and clears that line with `""`
   *  whenever a key does go somewhere. */
  readonly onReport: (line: string) => void;
  /** Unfolds the path down to one node and puts the focus on it. */
  readonly onReveal: (id: string) => void;
  /** Roots the tree at one node; the issue's own id is the whole tree. */
  readonly onZoom: (id: string) => void;
  /** Whether the tree is read one node at a time, which is how it is drawn at 1024 px and
   *  below: there the arrows that would walk into a branch zoom into it instead. */
  readonly oneNodeAtATime: boolean;
  /** The rows as drawn, in the order the keys move through them. */
  readonly rows: readonly DrawnRow[];
  /** The tree the preset left, which the keys that look past the drawn rows walk. */
  readonly root: TreeNode;
  /** The node the tree is rooted at: the issue itself while the reader has zoomed into
   *  nothing, and never undefined where the tree is read one node at a time. */
  readonly zoom: string | undefined;
}

/** One row of the tree that stands for a node. The one row a deeper branch folds to stands for
 *  no node and takes no focus, so the keys step from the row above it to the row below. */
type NodeRow = Extract<DrawnRow, { kind: "node" }>;

const NODE_ROW = "[data-node-row]";

/** How far a tree reaches when a key steps to its end. */
const END = Number.MAX_SAFE_INTEGER;

export function useTreeKeymap(tree: TreeKeyboard): void {
  // The row the caret was taken from, so Escape puts the reader back on it rather than on
  // whichever row the tree would otherwise offer.
  const caretFrom = useRef<HTMLElement | null>(null);

  /** The rows as the browser holds them, beside the rows as the fold drew them: the nth element
   *  is the nth node row, so one index answers for both. */
  const drawn = (): { elements: HTMLElement[]; rows: NodeRow[] } => ({
    elements: [...(tree.container.current?.querySelectorAll<HTMLElement>(NODE_ROW) ?? [])],
    rows: tree.rows.filter((row): row is NodeRow => row.kind === "node"),
  });
  /** Which row the reader is on, or -1 when the focus is not on one. */
  const at = (elements: readonly HTMLElement[]): number => {
    const active = document.activeElement;
    return active === null
      ? -1
      : elements.findIndex((row) => row === active || row.contains(active));
  };
  const inTheTree = (): boolean => {
    const active = document.activeElement;
    return active !== null && tree.container.current?.contains(active) === true;
  };
  /** Whether the reader is reading: on the page itself, or on a row of the tree. A key that
   *  moves the tree is not taken from a control somewhere else on the page. */
  const reading = (): boolean =>
    document.activeElement === null || document.activeElement === document.body || inTheTree();
  /** The node the reader is on, which is what the keys that act on one node ask for. */
  const focusedRow = (): NodeRow | undefined => {
    const { elements, rows } = drawn();
    return rows[at(elements)];
  };
  /** Moves to the row `index`, clamped to the tree's ends, and clears whatever a key reported. */
  const moveTo = (index: number): void => {
    const { elements } = drawn();
    if (elements.length === 0) return;
    tree.onReport("");
    elements[Math.min(Math.max(index, 0), elements.length - 1)]?.focus();
  };
  /** Takes the reader to `id`, unfolding whatever stands between them and it. */
  const goTo = (id: string): void => {
    tree.onReport("");
    tree.onReveal(id);
  };

  useKeymap("issue", [
    {
      // The tree is one tab stop (R9): Tab from the page itself enters it at the row the reader
      // last stood on, and Tab from inside it is the browser's own, which leaves the tree
      // because every row but that one takes no focus.
      id: "tree-enter",
      keys: ["Tab", "Shift+Tab"],
      label: "Move into the tree",
      palette: false,
      run: () => {
        const { elements } = drawn();
        const stop = elements.findIndex((row) => row.tabIndex === 0);
        moveTo(stop === -1 ? 0 : stop);
      },
      when: () =>
        (document.activeElement === null || document.activeElement === document.body) &&
        drawn().elements.length > 0,
    },
    {
      id: "tree-next",
      keys: ["j", "ArrowDown"],
      label: "Move to the next row",
      palette: false,
      run: () => {
        const { elements } = drawn();
        const on = at(elements);
        moveTo(on === -1 ? 0 : on + 1);
      },
      when: () => reading() && drawn().elements.length > 0,
    },
    {
      id: "tree-previous",
      keys: ["k", "ArrowUp"],
      label: "Move to the previous row",
      palette: false,
      run: () => {
        const { elements } = drawn();
        const on = at(elements);
        moveTo(on === -1 ? 0 : on - 1);
      },
      when: () => reading() && drawn().elements.length > 0,
    },
    {
      // Home and End are the page's own scroll keys everywhere else, so they move through the
      // tree only while the reader is standing in it.
      id: "tree-first",
      keys: "Home",
      label: "Move to the first row",
      palette: false,
      run: () => moveTo(0),
      when: inTheTree,
    },
    {
      id: "tree-last",
      keys: "End",
      label: "Move to the last row",
      palette: false,
      run: () => moveTo(END),
      when: inTheTree,
    },
    {
      // Right unfolds what is closed and then steps into it, which is the WAI-ARIA tree
      // pattern's rule for the key.
      id: "tree-unfold",
      keys: "ArrowRight",
      label: "Unfold the focused node, or move into it",
      palette: false,
      run: () => {
        const { elements, rows } = drawn();
        const on = at(elements);
        const row = rows[on];
        if (row === undefined) return;
        tree.onReport("");
        if (tree.oneNodeAtATime) {
          if (row.expanded !== undefined) tree.onZoom(row.node.id);
          return;
        }
        if (row.expanded === false) {
          tree.onFold(row.node.id, true);
          return;
        }
        if (row.expanded !== true) return;
        // A branch below the drawn levels folds to one line of its own, so the first child is
        // drawn only once this opens it in place.
        if ((rows[on + 1]?.level ?? 0) > row.level) moveTo(on + 1);
        else tree.onOpenDeeper(row.node.id);
      },
      when: () => at(drawn().elements) !== -1,
    },
    {
      id: "tree-fold",
      keys: "ArrowLeft",
      label: "Fold the focused node, or move to its parent",
      palette: false,
      run: () => {
        const { elements, rows } = drawn();
        const on = at(elements);
        const row = rows[on];
        if (row === undefined) return;
        tree.onReport("");
        if (tree.oneNodeAtATime) {
          if (row.parent !== undefined) tree.onZoom(row.parent);
          return;
        }
        if (row.expanded === true) {
          tree.onFold(row.node.id, false);
          return;
        }
        for (let above = on - 1; above >= 0; above -= 1) {
          if ((rows[above]?.level ?? 0) === row.level - 1) {
            moveTo(above);
            return;
          }
        }
      },
      when: () => at(drawn().elements) !== -1,
    },
    {
      id: "tree-pin",
      keys: "o",
      label: "Keep the focused node's body open",
      palette: false,
      run: () => {
        const row = focusedRow();
        if (row === undefined) return;
        tree.onReport("");
        tree.onPin(rowKeyOf(row));
      },
      when: () => at(drawn().elements) !== -1,
    },
    {
      id: "tree-zoom-in",
      keys: ".",
      label: "Zoom into the focused node",
      palette: false,
      run: () => {
        const row = focusedRow();
        if (row === undefined) return;
        tree.onReport("");
        tree.onZoom(row.node.id);
        tree.onReveal(row.node.id);
      },
      when: () => at(drawn().elements) !== -1,
    },
    {
      id: "tree-zoom-out",
      keys: ",",
      label: "Zoom back out one level",
      run: () => {
        const zoomed = tree.zoom;
        if (zoomed === undefined || zoomed === tree.root.id) {
          tree.onReport(KEY_REPORTS.whole_tree);
          return;
        }
        tree.onReport("");
        // Where the tree is read one node at a time the key walks the breadcrumb back up a
        // crumb at a time, so every node of the issue is reachable from any other; where the
        // whole tree is drawn there is nothing between a zoom and the issue, so it is the
        // issue the key comes back to.
        const above = pathTo(tree.root, zoomed)?.at(-2)?.id ?? tree.root.id;
        tree.onZoom(tree.oneNodeAtATime ? above : tree.root.id);
        tree.onReveal(zoomed);
      },
      when: reading,
    },
    {
      id: "tree-question",
      keys: "q",
      label: "Move to the next open question",
      run: () => {
        const question = nextQuestion(tree.root, focusedRow()?.node.id);
        if (question === undefined) {
          tree.onReport(KEY_REPORTS.no_next_question);
          return;
        }
        goTo(question.id);
      },
      when: reading,
    },
    {
      id: "tree-decision",
      keys: "w",
      label: "Move into the decision behind the focused node",
      run: () => {
        const node = focusedRow()?.node;
        const decision = node === undefined ? undefined : decisionBehind(tree.root, node.id);
        if (decision === undefined) {
          tree.onReport(KEY_REPORTS.no_decision);
          return;
        }
        goTo(decision.id);
      },
      when: reading,
    },
    {
      id: "tree-cycle",
      keys: "z",
      label: "Cycle how much of the tree is drawn",
      run: () => {
        tree.onReport("");
        tree.onCycle();
      },
      when: reading,
    },
    {
      // The caret goes into the node's own text where it lives, which is its document's block
      // drawn in this row (`IssueTree`'s `onEdit`); Escape below brings the reader back to
      // this row and gives the document back to whatever was drawing it.
      id: "tree-edit",
      keys: "Enter",
      label: "Put the caret in the focused node",
      palette: false,
      run: () => {
        const { elements, rows } = drawn();
        const on = at(elements);
        const row = rows[on];
        if (row === undefined) return;
        tree.onReport("");
        caretFrom.current = elements[on] ?? null;
        tree.onEdit(rowKeyOf(row), row.node);
      },
      when: () => at(drawn().elements) !== -1,
    },
    {
      // Escape leaves the caret for the row it came from, and leaves the tree from the row
      // (below). It extends the Escape the page already binds for its header controls, which
      // answers for a focused select and never for these two.
      id: "tree-leave-caret",
      inEditable: true,
      keys: "Escape",
      label: "Leave the node's text",
      palette: false,
      run: () => {
        const row = caretFrom.current;
        caretFrom.current = null;
        (document.activeElement as HTMLElement | null)?.blur();
        tree.onEditEnd();
        row?.focus();
      },
      when: () =>
        caretFrom.current !== null &&
        document.activeElement?.closest('[contenteditable="true"]') != null,
    },
    {
      id: "tree-leave",
      keys: "Escape",
      label: "Leave the tree",
      palette: false,
      run: () => (document.activeElement as HTMLElement | null)?.blur(),
      when: () => at(drawn().elements) !== -1,
    },
  ]);
}
