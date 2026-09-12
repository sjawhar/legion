import type { CreateProofEditorOptions, ProofEditorHandle } from "@sjawhar/proof-editor";

import type { BlockSchema } from "../../api/types";

export type { MarkAction, StoredMark } from "@sjawhar/proof-editor";

export type EditorHandle = ProofEditorHandle;
export type EditorOptions = CreateProofEditorOptions & { blockSchema: BlockSchema };
export type CreateEditor = (root: HTMLElement, options: EditorOptions) => Promise<EditorHandle>;

export const editorAttributes = {
  "aria-label": "Document editor",
  "aria-multiline": "true",
  role: "textbox",
} as const;

export const createEditor: CreateEditor = async (root, options) => {
  const [{ createProofEditor }] = await Promise.all([
    import("@sjawhar/proof-editor"),
    import("@sjawhar/proof-editor/style.css"),
  ]);
  const handle = await createProofEditor(root, options);
  for (const [name, value] of Object.entries(editorAttributes)) {
    handle.view.dom.setAttribute(name, value);
  }
  return handle;
};
