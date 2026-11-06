import type { CreateProofEditorOptions, ProofEditorHandle, StoredMark } from "@legion/proof-editor";
import type { QueryClient } from "@tanstack/react-query";
import type { Doc } from "yjs";

import type { BlockSchema } from "../../api/types";
import { requestMermaidDrawing } from "../refs/mermaid-diagrams";
import { importWhenOnline } from "../shell/DeploymentResilience";
import type { WatchPicture } from "./picture-view";
import type { NavigateToReference } from "./reference-click";
import { MARKS_MAP_NAME } from "./roots";
import type { AskBlockHost } from "./typed-block-drawing";

export type {
  MarkAction,
  RecordMarkPlacement,
  RemarkOutcome,
  RemarkRefusal,
  RetypedMark,
  RetypeOutcome,
  RetypeRefusal,
  StoredMark,
} from "@legion/proof-editor";

export type EditorHandle = ProofEditorHandle;
export type EditorOptions = CreateProofEditorOptions & {
  blockSchema: BlockSchema;
  /** The document's decision blocks, in document order, whenever the set changes. */
  onAskBlockHosts: (hosts: readonly AskBlockHost[]) => void;
  /** How the picture node view's fallback link opens its artifact inside the app, and the
   *  client the one path it cannot build on its own is read through (`reference-click.ts`).
   *  Plain DOM holds no hook, so the mount hands both down; whether the view takes edits is
   *  the editor's own live state, never this mount's `readOnly`, since `setReadOnly` changes
   *  it in place and rebuilds no node view. */
  navigate: NavigateToReference;
  queryClient: QueryClient;
  /** What a version-less picture's latest version is read through, for the `image` node view
   *  `installTypedBlocks` mounts below. The reading is the page's (`watchPicturePath`, bound to
   *  its query client), so a picture shares one summary read with the links and cards beside it;
   *  the caller passes it because only React holds that client. */
  watchPicture: WatchPicture;
};
export type CreateEditor = (root: HTMLElement, options: EditorOptions) => Promise<EditorHandle>;

export const editorAttributes = {
  "aria-label": "Document editor",
  "aria-multiline": "true",
  role: "textbox",
} as const;

// The editor library sits behind `import()` so the issue route ships none of it until a
// document mounts; `importWhenOnline` holds the retry policy for a chunk that fails offline.
// Every editor's mermaid blocks (the live document, a read-only one, a past version) draw through
// `requestMermaidDrawing`, in the Mermaid worker comments use, never on the page's thread.
//
// Dispatch's own node views travel with it (`ask-block.ts`): they are written against
// ProseMirror's `DOMSerializer`, `Plugin` and `PluginKey`, so a page that imported them to call
// them after the editor existed would parse a quarter of a megabyte of the editor's library
// before its first paint - which is the very thing this `import()` is here to prevent.
export const createEditor: CreateEditor = async (root, options) => {
  // `picture-view.ts` is in this chunk already (`ask-block.ts` mounts its node view), so taking
  // the card sync from the same `import()` costs nothing and keeps the boundary: a static import
  // of it here would pull ProseMirror's model and state onto the page before its first paint.
  const [{ createProofEditor }, { installTypedBlocks }, { syncFallbackLinkCards }] =
    await importWhenOnline(() =>
      Promise.all([
        import("@legion/proof-editor"),
        import("./ask-block"),
        import("./picture-view"),
        import("@legion/proof-editor/style.css"),
      ])
    );
  const handle = await createProofEditor(root, { ...options, drawMermaid: requestMermaidDrawing });
  for (const [name, value] of Object.entries(editorAttributes)) {
    handle.view.dom.setAttribute(name, value);
  }
  installTypedBlocks(
    handle.view,
    options.blockSchema,
    options.onAskBlockHosts,
    options.watchPicture,
    {
      editable: () => handle.view.editable,
      navigate: options.navigate,
      queryClient: options.queryClient,
    }
  );
  return {
    ...handle,
    // `setReadOnly` is the one place the editor's editable state changes after the mount, and
    // ProseMirror rebuilds no node view for it. A link drawn after the change reads the live
    // state itself (`editable` above); the links already drawn are written here, from the same
    // `view.editable`, so each one carries Dispatch's card in a read-only view and the editor
    // library's own in an editable one - one card, at any moment (`syncFallbackLinkCards`).
    setReadOnly(readOnly: boolean) {
      handle.setReadOnly(readOnly);
      syncFallbackLinkCards(handle.view);
    },
  };
};

// bindRemoteMarks projects the document's `marks` map into the editor now and after every
// transaction that changes it, and returns the unbinding. The projection waits for the end of the
// transaction: Yjs calls the map's observers before the deep observer through which y-prosemirror
// draws the same transaction's text, so projecting from the map's observer sees the text before
// the update and writes it back (a remote accept of a suggestion, arriving with its projection,
// reverts). The observer only notes that the map changed.
export function bindRemoteMarks(doc: Doc, handle: EditorHandle): () => void {
  const marks = doc.getMap(MARKS_MAP_NAME);
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
