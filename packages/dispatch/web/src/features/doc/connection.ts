import { HocuspocusProvider } from "@hocuspocus/provider";
import type { Awareness } from "y-protocols/awareness";
import * as Y from "yjs";

const presenceColors = ["#0284c7", "#7c3aed", "#c2410c", "#047857", "#be123c", "#4338ca"] as const;

export type ConnectionState = "connecting" | "connected" | "offline";

export function connectionLabel(connection: ConnectionState): string {
  return connection === "connecting" ? "Connecting to the document…" : connection;
}

export interface DocumentConnection {
  readonly awareness: Awareness;
  readonly doc: Y.Doc;
  destroy(): void;
}

export interface ConnectionCallbacks {
  onStatus(state: ConnectionState): void;
  onSynced(): void;
}

export type ConnectDocument = (
  artifactId: string,
  callbacks: ConnectionCallbacks
) => DocumentConnection;

export function colorForLogin(login: string): string {
  let hash = 0;
  for (const character of login) {
    hash = (hash * 31 + character.charCodeAt(0)) | 0;
  }
  return presenceColors[Math.abs(hash) % presenceColors.length] ?? presenceColors[0];
}

export function wsUrl(artifactId: string): string {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}/ws/doc/${encodeURIComponent(artifactId)}`;
}

export const connectDocument: ConnectDocument = (artifactId, callbacks) => {
  const doc = new Y.Doc();
  const provider = new HocuspocusProvider({
    document: doc,
    name: artifactId,
    onStatus: ({ status }) => callbacks.onStatus(status === "disconnected" ? "offline" : status),
    onSynced: ({ state }) => {
      if (state) {
        callbacks.onSynced();
      }
    },
    url: wsUrl(artifactId),
  });
  const awareness = provider.awareness;
  if (awareness === null) {
    provider.destroy();
    doc.destroy();
    throw new Error("Dispatch document provider did not create awareness.");
  }
  return {
    awareness,
    destroy() {
      provider.destroy();
      doc.destroy();
    },
    doc,
  };
};
