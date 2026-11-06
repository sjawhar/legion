import { type RefObject, useCallback, useEffect, useRef, useState } from "react";

import type { Artifact } from "../../api/types";
import { useMargin } from "../margin/margin-context";
import { documentOfNode } from "./issue-artifacts";
import type { TreeNode } from "./issue-tree";
import { caretInBlock } from "./node-caret";
import { KEY_REPORTS } from "./page-structure";

/** One row the reader is editing in: the document its node's text lives in, the block of that
 *  document the node is, and which row of which node is holding the editor. */
export interface EditedRow {
  readonly artifactId: string;
  readonly blockId?: string;
  readonly node: string;
  readonly row: string;
}

export interface NodeEditing {
  /** The row holding the editor, or `undefined` while the reader is editing nothing. */
  readonly editing: EditedRow | undefined;
  /** Puts the caret in one row's node, in the document that node's text lives in. */
  readonly edit: (row: string, node: TreeNode) => void;
  /** Takes the caret out again and gives the row the focus back. */
  readonly stopEditing: () => void;
}

/**
 * The caret in a node's own text, where that text lives: the node's block of its own document,
 * with the collaborative cursors and the comment chord it has there (dispatch://INFRA-1196 R9,
 * the Design section's "Editing").
 *
 * The tree holds no editor of its own. The page draws the one editor of whichever document a
 * row is editing (`IssuePage`, through `onEditing`), `document-stage.tsx` moves it onto the
 * row, and this places the caret in the node's own block of it (`node-caret.ts`) one frame
 * later, because the page mounts that editor in the commit after the row asked for it. A node
 * whose text no document of this issue holds has nothing to put a caret in, and the key says so.
 *
 * Escape leaves the caret for the row it came from. The editor calls `preventDefault` on the
 * key before it reaches the page's keyboard registry, which drops every binding of the press
 * (`features/shell/keymap.ts`'s `handleKeyDown`), so the row listens for it in the capture
 * phase while it is being edited in - before the editor sees it - and the registry's own
 * binding stands for every other surface (`useTreeKeymap`'s `tree-leave-caret`). The editor
 * then asserts its own selection again a moment after the blur (`prosemirror-view`'s focus
 * handler) and its stage moves off the row in the same commit, so the row takes the focus once
 * more on the next frame as well as the moment Escape was pressed.
 */
export function useNodeEditing({
  artifacts,
  container,
  onEditing,
  onReport,
}: {
  /** Each artifact of the issue by its slug (`issue-artifacts.ts`). */
  artifacts: ReadonlyMap<string, Artifact>;
  /** The tree's own box, which the rows are found inside. */
  container: RefObject<HTMLElement | null>;
  /** Tells the page which document a row is editing, so it draws that document's one editor. */
  onEditing: (artifactId: string | undefined) => void;
  /** What a key reports when the node it was pressed on has no text to put a caret in. */
  onReport: (report: string) => void;
}): NodeEditing {
  const [editing, setEditing] = useState<EditedRow | undefined>(undefined);
  // The node the reader has just left the caret in, so the row takes the focus back.
  const leaving = useRef<string | undefined>(undefined);
  const { documentBridge } = useMargin();

  const edit = useCallback(
    (row: string, node: TreeNode) => {
      const place = documentOfNode(node.id, artifacts);
      if (place === undefined) {
        onReport(KEY_REPORTS.no_text);
        return;
      }
      setEditing({
        artifactId: place.artifact.id,
        ...(place.blockId === undefined ? {} : { blockId: place.blockId }),
        node: node.id,
        row,
      });
    },
    [artifacts, onReport]
  );
  const stopEditing = useCallback(() => {
    setEditing((held) => {
      if (held !== undefined) leaving.current = held.node;
      return undefined;
    });
  }, []);

  const editingArtifact = editing?.artifactId;
  const editingBlock = editing?.blockId;
  const editingNode = editing?.node;

  useEffect(() => {
    onEditing(editingArtifact);
  }, [editingArtifact, onEditing]);

  useEffect(() => {
    if (editingNode === undefined) return;
    const frame = requestAnimationFrame(() => {
      const host = container.current?.querySelector<HTMLElement>(
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
  }, [container, documentBridge, editingArtifact, editingBlock, editingNode]);

  useEffect(() => {
    const box = container.current;
    if (editingNode === undefined || box === null) return;
    const leave = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      event.preventDefault();
      (document.activeElement as HTMLElement | null)?.blur();
      box
        .querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(editingNode)}"]`)
        ?.focus();
      stopEditing();
    };
    box.addEventListener("keydown", leave, true);
    return () => box.removeEventListener("keydown", leave, true);
  }, [container, editingNode, stopEditing]);

  useEffect(() => {
    const left = leaving.current;
    if (editingNode !== undefined || left === undefined) return;
    leaving.current = undefined;
    const frame = requestAnimationFrame(() => {
      container.current
        ?.querySelector<HTMLElement>(`[data-node-row][data-node-id="${CSS.escape(left)}"]`)
        ?.focus();
    });
    return () => cancelAnimationFrame(frame);
  }, [container, editingNode]);

  return { edit, editing, stopEditing };
}
