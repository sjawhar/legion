import type { HocuspocusProvider } from "@hocuspocus/provider";
import type { Awareness } from "y-protocols/awareness";
import type { Doc } from "yjs";

import { importWhenOnline } from "../shell/DeploymentResilience";

const presenceColors = ["#0284c7", "#7c3aed", "#c2410c", "#047857", "#be123c", "#4338ca"] as const;

export type ConnectionState = "connecting" | "connected" | "offline" | "failed";

export function connectionLabel(connection: ConnectionState): string {
  switch (connection) {
    case "connecting":
      return "Connecting to the document…";
    case "failed":
      return "The document could not load";
    default:
      return connection;
  }
}

export interface DocumentConnection {
  readonly awareness: Awareness;
  readonly doc: Doc;
  destroy(): void;
}

export interface ConnectionCallbacks {
  schemaVersion: number;
  onAdmission(readOnly: boolean): void;
  onStatus(state: ConnectionState): void;
  /** Fires once, when the server's first sync completes after admission. */
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

export function isSchemaReadOnly(scope: string | undefined): boolean {
  return scope === "readonly";
}

export function wsUrl(artifactId: string): string {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${protocol}//${window.location.host}/ws/doc/${encodeURIComponent(artifactId)}`;
}

// Hocuspocus and Yjs load with the first document that connects, not with the issue route: the
// same lazy boundary `createEditor` puts around the editor itself. The connect they resolve to is
// synchronous, so a host has one place to react to the connection.
export async function loadDocumentTransport(): Promise<ConnectDocument> {
  const [{ HocuspocusProvider }, { Doc }] = await importWhenOnline(() =>
    Promise.all([import("@hocuspocus/provider"), import("yjs")])
  );
  return (artifactId, callbacks) => {
    const doc = new Doc();
    let synced = false;
    let authenticated = false;
    let announced = false;
    let provider: HocuspocusProvider;
    const notifySynced = () => {
      if (synced && authenticated && !announced) {
        announced = true;
        callbacks.onSynced();
      }
    };
    provider = new HocuspocusProvider({
      document: doc,
      name: artifactId,
      onAuthenticated: () => {
        authenticated = true;
        callbacks.onAdmission(isSchemaReadOnly(provider.authorizedScope));
        notifySynced();
      },
      onStatus: ({ status }) => callbacks.onStatus(status === "disconnected" ? "offline" : status),
      onSynced: ({ state }) => {
        if (state) {
          synced = true;
          notifySynced();
        }
      },
      parameters: { schema_version: callbacks.schemaVersion },
      // Each document owns its socket, so destroying the provider must close it. Hocuspocus
      // defaults `preserveConnection` to true — meant for a socket shared by several documents —
      // which leaves `destroy()` detaching the document while the socket stays open with
      // `shouldConnect` set: the server's close of the abandoned room is then answered with an
      // immediate reconnect, and a reader moving between documents accumulates one live room per
      // visit for the life of the tab.
      preserveConnection: false,
      token: String(callbacks.schemaVersion),
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
}
