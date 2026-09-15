import type { CreateProofEditorOptions, ProofEditorHandle } from "@sjawhar/proof-editor";
import type { Doc } from "yjs";

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

// The editor, Yjs, and Hocuspocus are code-split so the issue route does not pay for them until
// a document mounts. A chunk that fails to download while the browser is offline is retried once
// the network returns; Chromium caches a failed module fetch in its module map, so that retry can
// reject again, in which case the failure propagates like an online one — `DeploymentResilience`
// treats it as a replaced deployment and reloads once per session.
export async function importWhenOnline<T>(load: () => Promise<T>): Promise<T> {
  for (;;) {
    try {
      return await load();
    } catch (error) {
      if (navigator.onLine) {
        throw error;
      }
      await new Promise<void>((resolve) => {
        window.addEventListener("online", () => resolve(), { once: true });
      });
    }
  }
}

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
