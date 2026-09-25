import type {
  CreateProofEditorOptions,
  ProofEditorHandle,
  StoredMark,
} from "@sjawhar/proof-editor";
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

// bindRemoteMarks projects the document's `marks` map into the editor now and after every
// transaction that changes it, and returns the unbinding. The projection waits for the end of the
// transaction: Yjs calls the map's observers before the deep observer through which y-prosemirror
// draws the same transaction's text, so projecting from the map's observer sees the text before
// the update and writes it back (a remote accept of a suggestion, arriving with its projection,
// reverts). The observer only notes that the map changed.
export function bindRemoteMarks(doc: Doc, handle: EditorHandle): () => void {
  const marks = doc.getMap("marks");
  const project = () => {
    handle.applyRemoteMarks(marks.toJSON() as Record<string, StoredMark>, {
      hydrateAnchors: false,
    });
  };
  let changed = false;
  const noteChange = () => {
    changed = true;
  };
  const projectChange = () => {
    if (changed) {
      changed = false;
      project();
    }
  };
  project();
  marks.observe(noteChange);
  doc.on("afterTransaction", projectChange);
  return () => {
    marks.unobserve(noteChange);
    doc.off("afterTransaction", projectChange);
  };
}

// A standalone Yjs document for an editor with no live connection (a historical version).
export async function createDoc(): Promise<Doc> {
  const { Doc } = await importWhenOnline(() => import("yjs"));
  return new Doc();
}
