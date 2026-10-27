import { createContext } from "react";

import { type ConnectDocument, loadDocumentTransport } from "./connection";
import { type CreateEditor, createEditor } from "./editor";

export interface DocumentRuntimeValue {
  loadTransport(): Promise<ConnectDocument>;
  createEditor: CreateEditor;
}

export const DocumentRuntime = createContext<DocumentRuntimeValue>({
  loadTransport: loadDocumentTransport,
  createEditor,
});
