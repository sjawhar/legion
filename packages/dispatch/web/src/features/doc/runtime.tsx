import { createContext } from "react";

import { type ConnectDocument, connectDocument } from "./connection";
import { type CreateEditor, createEditor } from "./editor";

export interface DocumentRuntimeValue {
  connect: ConnectDocument;
  createEditor: CreateEditor;
}

export const DocumentRuntime = createContext<DocumentRuntimeValue>({
  connect: connectDocument,
  createEditor,
});
