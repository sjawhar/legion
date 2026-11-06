import { createContext } from "react";

import type { BlockSchema } from "../../api/types";
import { type ConnectDocument, loadDocumentTransport } from "./connection";
import { type CreateEditor, createEditor, loadEditor } from "./editor";

export interface DocumentRuntimeValue {
  loadTransport(): Promise<ConnectDocument>;
  /** Starts the editor chunk's download, without building one (`loadEditor`). */
  loadEditor(): void;
  createEditor: CreateEditor;
  blockSchema?: BlockSchema;
}

export const DocumentRuntime = createContext<DocumentRuntimeValue>({
  loadTransport: loadDocumentTransport,
  loadEditor,
  createEditor,
});
