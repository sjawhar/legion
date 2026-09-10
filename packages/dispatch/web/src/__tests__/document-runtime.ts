import { Awareness } from "y-protocols/awareness";
import * as Y from "yjs";

import type {
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
import type { DocumentRuntimeValue } from "../features/doc/runtime";

export interface FakeEditor {
  destroyed: boolean;
  focused: string[];
  markdown: string | undefined;
  options: EditorOptions;
  readOnly: boolean;
  remoteMarks: Record<string, StoredMark>[];
  root: HTMLElement;
}

interface FakeConnection extends DocumentConnection {
  destroyed: boolean;
}

export interface FakeDocumentRuntime {
  connections: FakeConnection[];
  editors: FakeEditor[];
  runtime: DocumentRuntimeValue;
  status(state: ConnectionState): void;
  sync(): void;
}

export function fakeDocumentRuntime(seed: { text?: string } = {}): FakeDocumentRuntime {
  const callbacks: ConnectionCallbacks[] = [];
  const connections: FakeConnection[] = [];
  const editors: FakeEditor[] = [];
  const connect = (_artifactId: string, nextCallbacks: ConnectionCallbacks): DocumentConnection => {
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
      destroyed: false,
      focused: [],
      markdown: undefined,
      options,
      readOnly: options.readOnly ?? false,
      remoteMarks: [],
      root,
    };
    root.textContent = options.ydoc.getXmlFragment("prosemirror").toString();
    const transaction = {
      docChanged: false,
      removeMark() {
        return transaction;
      },
    };
    const handle = {
      applyRemoteMarks(metadata: Record<string, StoredMark>) {
        editor.remoteMarks.push(metadata);
      },
      destroy() {
        editor.destroyed = true;
      },
      focusMark(markId: string) {
        editor.focused.push(markId);
      },
      getMarkdown: () => editor.markdown ?? root.textContent ?? "",
      removeMark() {},
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
        state: { doc: { descendants() {} }, tr: transaction },
      },
    } as unknown as EditorHandle;
    editors.push(editor);
    return handle;
  };

  return {
    connections,
    editors,
    runtime: { connect, createEditor },
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
  };
}
