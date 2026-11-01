import { createContext } from "react";

import type { BlockSchema } from "../../api/types";
import { type ConnectDocument, loadDocumentTransport } from "./connection";
import { type CreateEditor, createEditor } from "./editor";

export interface DocumentRuntimeValue {
  loadTransport(): Promise<ConnectDocument>;
  createEditor: CreateEditor;
  blockSchema?: BlockSchema;
}

export const DocumentRuntime = createContext<DocumentRuntimeValue>({
  loadTransport: loadDocumentTransport,
  createEditor,
});
