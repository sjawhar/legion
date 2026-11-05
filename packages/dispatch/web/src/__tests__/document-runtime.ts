import type { BlockMark, DocumentJSON } from "@legion/proof-editor";
import { Awareness } from "y-protocols/awareness";
import * as Y from "yjs";

import { installTypedBlocks } from "../features/doc/ask-block";
import type {
  ConnectDocument,
  ConnectionCallbacks,
  ConnectionState,
  DocumentConnection,
} from "../features/doc/connection";
import {
  type CreateEditor,
  type EditorHandle,
  type EditorOptions,
  editorAttributes,
  type StoredMark,
} from "../features/doc/editor";
import type { PendingState } from "../features/doc/pending-sync";
import type { DocumentRuntimeValue } from "../features/doc/runtime";

export interface FakeEditor {
  /** Each set of block marks the host handed the editor (`setBlockMarks`), in order. */
  blockMarks: BlockMark[][];
  /** How many times the host asked to comment on the selection. */
  commented: number;
  destroyed: boolean;
  /** The document the host drew with `setDocumentJSON`: the tree its server read, which the
   *  fake holds as it came rather than parsing anything. */
  documentJSON: DocumentJSON | undefined;
  focused: string[];
  markdown: string | undefined;
  focusedBlocks: string[];
  options: EditorOptions;
  readOnly: boolean;
  remoteMarks: Record<string, StoredMark>[];
  root: HTMLElement;
  /** Whether the fake view's selection is empty (a caret); a test sets it to select text. */
  selectionEmpty: boolean;
  /** The innermost block with an id that holds the selection, as `blockIdAtSelection` answers;
   *  a test sets it to copy a block link. */
  selectedBlock: string | null;
  /** The fake view's props bag, where `setProps` accumulates node views the host installs. */
  viewProps: Record<string, unknown>;
}

interface FakeConnection extends DocumentConnection {
  destroyed: boolean;
}

export interface FakeDocumentRuntime {
  admit(readOnly: boolean): void;
  connections: FakeConnection[];
  editors: FakeEditor[];
  pending(state: PendingState): void;
  /** The server refuses the current socket because the stored document is outside the Proof
   * schema; a destroyed provider hears nothing more, so earlier connections are not told. */
  refuse(): void;
  runtime: DocumentRuntimeValue;
  status(state: ConnectionState): void;
  sync(): void;
  text: string;
}

const blockSchema = {
  types: [
    {
      attributes: {
        kind: { choices: ["note", "warning"], default: "note", kind: "enum" as const },
        title: { default: "", kind: "string" as const },
      },
      content: "paragraph+" as const,
      name: "callout",
      render: "host" as const,
    },
  ],
  version: 1,
};

/** The words of a document as JSON, its top-level blocks a line each: what the fake editor puts
 *  into its root for a document the host drew, as `setMarkdown` puts the markdown it was
 *  handed. */
function textOf(tree: DocumentJSON): string {
  if (tree.text !== undefined) return tree.text;
  return (tree.content ?? []).map(textOf).join(tree.type === "doc" ? "\n" : "");
}

/** The text of every run of `tree` carrying a mark of `type`, in document order: what a mark a
 *  host put into the document it drew reads over. */
export function markedText(tree: DocumentJSON | undefined, type: string): string[] {
  if (tree === undefined) return [];
  if (tree.marks?.some((mark) => mark.type === type) === true && tree.text !== undefined) {
    return [tree.text];
  }
  return (tree.content ?? []).flatMap((child) => markedText(child, type));
}

export function fakeDocumentRuntime(seed: { text?: string } = {}): FakeDocumentRuntime {
  const callbacks: ConnectionCallbacks[] = [];
  const connections: FakeConnection[] = [];
  const editors: FakeEditor[] = [];
  const connect: ConnectDocument = (_artifactId, nextCallbacks) => {
    const doc = new Y.Doc();
    if (seed.text !== undefined) {
      const paragraph = new Y.XmlElement("paragraph");
      paragraph.insert(0, [new Y.XmlText(seed.text)]);
      doc.getXmlFragment("prosemirror").insert(0, [paragraph]);
    }
    const connection: FakeConnection = {
      awareness: new Awareness(doc),
      destroyed: false,
      destroy() {
        connection.destroyed = true;
      },
      doc,
    };
    callbacks.push(nextCallbacks);
    connections.push(connection);
    return connection;
  };
  const createEditor: CreateEditor = async (root, options) => {
    for (const [name, value] of Object.entries(editorAttributes)) {
      root.setAttribute(name, value);
    }
    const editor: FakeEditor = {
      blockMarks: [],
      commented: 0,
      destroyed: false,
      documentJSON: undefined,
      focused: [],
      focusedBlocks: [],
      markdown: undefined,
      options,
      readOnly: options.readOnly ?? false,
      remoteMarks: [],
      root,
      selectionEmpty: true,
      selectedBlock: null,
      viewProps: { nodeViews: {} },
    };
    root.textContent = options.ydoc.getXmlFragment("prosemirror").toString();
    const handle = {
      applyRemoteMarks(metadata: Record<string, StoredMark>) {
        editor.remoteMarks.push(metadata);
      },
      blockIdAtSelection() {
        return editor.selectedBlock;
      },
      commentOnSelection() {
        editor.commented += 1;
        return true;
      },
      destroy() {
        editor.destroyed = true;
      },
      focusBlock(blockId: string) {
        editor.focusedBlocks.push(blockId);
      },
      focusMark(markId: string) {
        editor.focused.push(markId);
      },
      getMarkdown: () => editor.markdown ?? root.textContent ?? "",
      markOffsets() {
        return new Map<string, number>();
      },
      removeMark() {},
      remarkMark() {
        return { refused: "gone" as const };
      },
      retypeMark() {
        return { refused: "missing" as const };
      },
      setActiveBlocks() {},
      setBlockMarks(marks: readonly BlockMark[]) {
        editor.blockMarks.push([...marks]);
      },
      setActiveMarks() {},
      setDocumentJSON(tree: DocumentJSON) {
        editor.documentJSON = tree;
        root.textContent = textOf(tree);
      },
      setMarkdown(markdown: string) {
        editor.markdown = markdown;
        root.textContent = markdown;
      },
      setReadOnly(readOnly: boolean) {
        editor.readOnly = readOnly;
      },
      view: {
        dispatch() {},
        dom: root,
        get editable() {
          return !editor.readOnly;
        },
        hasFocus: () => root.ownerDocument.activeElement === root,
        props: editor.viewProps,
        setProps(next: Record<string, unknown>) {
          Object.assign(editor.viewProps, next);
        },
        state: {
          doc: { descendants() {} },
          selection: {
            get empty() {
              return editor.selectionEmpty;
            },
          },
        },
      },
    } as unknown as EditorHandle;
    // The real `createEditor` installs Dispatch's node views once the editor library has
    // loaded (`features/doc/editor.ts`); the fake stands in for that call too, so a test reads
    // the same `viewProps.nodeViews` the host would.
    installTypedBlocks(
      handle.view,
      options.blockSchema,
      options.onAskBlockHosts,
      options.watchPicture
    );
    editors.push(editor);
    return handle;
  };

  return {
    admit(readOnly) {
      for (const callback of callbacks) {
        callback.onAdmission(readOnly);
      }
    },
    connections,
    editors,
    pending(state) {
      for (const callback of callbacks) {
        callback.onPending(state);
      }
    },
    refuse() {
      callbacks.at(-1)?.onOutsideSchema();
    },
    runtime: { blockSchema, createEditor, loadTransport: async () => connect },
    status(state) {
      for (const callback of callbacks) {
        callback.onStatus(state);
      }
    },
    sync() {
      for (const callback of callbacks) {
        callback.onSynced();
      }
    },
    text: seed.text ?? "",
  };
}
