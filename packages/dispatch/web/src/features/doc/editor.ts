import type { CreateProofEditorOptions, ProofEditorHandle } from "@sjawhar/proof-editor";
import type { Doc } from "yjs";

import type { BlockSchema } from "../../api/types";
import { importWhenOnline } from "../shell/DeploymentResilience";

export type { MarkAction, StoredMark } from "@sjawhar/proof-editor";

export type EditorHandle = ProofEditorHandle;
export type EditorOptions = CreateProofEditorOptions & { blockSchema: BlockSchema };
export type CreateEditor = (root: HTMLElement, options: EditorOptions) => Promise<EditorHandle>;

export const editorAttributes = {
  "aria-label": "Document editor",
  "aria-multiline": "true",
  role: "textbox",
} as const;

// The editor library sits behind `import()` so the issue route ships none of it until a
// document mounts; `importWhenOnline` holds the retry policy for a chunk that fails offline.
export const createEditor: CreateEditor = async (root, options) => {
  const [{ createProofEditor }] = await importWhenOnline(() =>
    Promise.all([import("@sjawhar/proof-editor"), import("@sjawhar/proof-editor/style.css")])
  );
  const handle = await createProofEditor(root, options);
  for (const [name, value] of Object.entries(editorAttributes)) {
    handle.view.dom.setAttribute(name, value);
  }
  return handle;
};

// A standalone Yjs document for an editor with no live connection (a historical version).
export async function createDoc(): Promise<Doc> {
  const { Doc } = await importWhenOnline(() => import("yjs"));
  return new Doc();
}
