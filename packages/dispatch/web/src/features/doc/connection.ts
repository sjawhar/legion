import type { HocuspocusProvider } from "@hocuspocus/provider";
import { DOCUMENT_SCHEMA_CLOSE_CODE } from "@legion/contracts";
import type { Awareness } from "y-protocols/awareness";
import type { Doc } from "yjs";

import { importWhenOnline } from "../shell/DeploymentResilience";
import type { PendingState, PendingSync } from "./pending-sync";

const presenceColors = ["#0284c7", "#7c3aed", "#c2410c", "#047857", "#be123c", "#4338ca"] as const;

export type ConnectionState = "connecting" | "connected" | "offline" | "failed";
function savedEdits(count: number): string {
  return count === 1 ? "1 edit" : `${count} edits`;
}

export function pendingNotice(
  connection: ConnectionState,
  pending: PendingState | undefined
): string | undefined {
  if (pending === undefined) {
    return undefined;
  }
  if (pending.rebuiltAt !== undefined) {
    return `Edits saved in this browser at ${new Date(pending.rebuiltAt).toLocaleString()} could not be applied: the document was rebuilt since.`;
  }
  if (pending.readOnly && pending.count > 0) {
    return `${savedEdits(pending.count)} saved in this browser can't be sent: this document is read-only.`;
  }
  if (connection === "offline" || connection === "connecting") {
    if (!pending.stored) {
      return "Edits typed now will not survive a reload.";
    }
    if (pending.count > 0) {
      return `${savedEdits(pending.count)} saved in this browser, not sent yet.`;
    }
  }
  return undefined;
}

/** The connection's name, followed by the pending-edit notice when there is one. */
export function connectionLabel(
  connection: ConnectionState,
  notice: string | undefined = undefined
): string {
  let label: string;
  switch (connection) {
    case "connecting":
      label = "Connecting to the document…";
      break;
    case "failed":
      label = "The document could not load";
      break;
    default:
      label = connection;
  }
  return notice === undefined ? label : `${label} · ${notice}`;
}

export interface DocumentConnection {
  readonly awareness: Awareness;
  readonly doc: Doc;
  destroy(): void;
}

export interface ConnectionCallbacks {
  schemaVersion: number;
  onAdmission(readOnly: boolean): void;
  /**
   * Fires when the server refuses the socket because the stored document is outside the Proof
   * schema. The connection has stopped reconnecting; a fresh read decides what comes next.
   */
  onOutsideSchema(): void;
  onStatus(state: ConnectionState): void;
  /** Reports browser-held edits as IndexedDB changes or this admission's restore completes. */
  onPending(state: PendingState): void;
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
  const [
    { HocuspocusProvider, HocuspocusProviderWebsocket },
    { Doc },
    { openPendingEdits },
    { startPendingSync },
  ] = await importWhenOnline(() =>
    Promise.all([
      import("@hocuspocus/provider"),
      import("yjs"),
      import("./pending-edits"),
      import("./pending-sync"),
    ])
  );
  return (artifactId, callbacks) => {
    const doc = new Doc();
    let destroyed = false;
    // Whether the current socket's SyncStep2 has been applied and its admission authenticated. A
    // close ends the socket's round, so both start over with the reconnect's.
    let synced = false;
    let authenticated = false;
    // Whether the current socket's admission has been handed to the pending sync.
    let admitted = false;
    // Whether any admission has: the first is the connection's first sync, the rest are later ones.
    let announced = false;
    let provider: HocuspocusProvider;
    // `connect: false` below means this class cannot instantiate a socket until `sync` is assigned
    // after the provider exists; the explicit `websocket.connect()` is deliberately last.
    let sync: PendingSync | undefined;
    const WebSocketPolyfill = class extends globalThis.WebSocket {
      constructor(url: string, protocols?: string | string[]) {
        super(url, protocols);
        this.addEventListener("open", () => sync?.socketOpened());
      }

      override send(data: Parameters<WebSocket["send"]>[0]): void {
        super.send(data);
        if (data instanceof Uint8Array) {
          sync?.frameWritten(data);
        }
      }
    };
    const websocket = new HocuspocusProviderWebsocket({
      WebSocketPolyfill,
      connect: false,
      parameters: { schema_version: callbacks.schemaVersion },
      url: wsUrl(artifactId),
    });
    // Every admission waits for its own socket's authentication and SyncStep2 alike. A writable
    // one restores the rows other tabs stored, judging each against this document; before the
    // reconnect's sync applies, the document lacks what the server took meanwhile, so a row that
    // builds on it would look as if it were typed against a rebuilt history and be dropped.
    const notifyAdmitted = () => {
      if (!synced || !authenticated || admitted) {
        return;
      }
      admitted = true;
      const readOnly = isSchemaReadOnly(provider.authorizedScope);
      if (announced) {
        // A reconnect's admission can differ from the first one (an issue reopened or closed
        // since), and the pending sync decides from it whether a local update is an edit to keep.
        void sync?.admitted(readOnly).catch((error: unknown) => {
          console.error("Could not take this document's new admission", error);
        });
        return;
      }
      announced = true;
      void sync
        ?.firstSync(readOnly)
        .catch((error: unknown) => {
          console.error("Could not restore this document's pending browser edits", error);
        })
        .then(() => {
          if (!destroyed) {
            callbacks.onSynced();
          }
        });
    };
    provider = new HocuspocusProvider({
      document: doc,
      name: artifactId,
      onAuthenticated: () => {
        authenticated = true;
        callbacks.onAdmission(isSchemaReadOnly(provider.authorizedScope));
        notifyAdmitted();
      },
      onClose: ({ event }) => {
        // A closed socket's admission is over; the reconnect authenticates and syncs anew, and its
        // own admission waits for both (`synced` the provider resets through `onSynced`).
        authenticated = false;
        admitted = false;
        // Hocuspocus answers every other close by reconnecting. This one is the server's decision
        // about the stored document (`DOCUMENT_SCHEMA_CLOSE_CODE`), so the provider stops here and
        // the host reads it again.
        if (event.code === DOCUMENT_SCHEMA_CLOSE_CODE) {
          provider.disconnect();
          callbacks.onOutsideSchema();
        }
      },
      onMessage: ({ event }) => {
        if (event.data instanceof ArrayBuffer) {
          void sync?.frameReceived(new Uint8Array(event.data));
        }
      },
      onStatus: ({ status }) => callbacks.onStatus(status === "disconnected" ? "offline" : status),
      onSynced: ({ state }) => {
        // The provider reports `false` as a socket closes, before `onClose` above runs, and `true`
        // once the socket's SyncStep2 has been applied to the document.
        synced = state;
        notifyAdmitted();
      },
      // Each document owns its socket. A transport's destroy closes this provider and then the
      // websocket itself, so its connection checker and reconnect attempt cannot outlive it.
      preserveConnection: false,
      token: String(callbacks.schemaVersion),
      websocketProvider: websocket,
    });
    const awareness = provider.awareness;
    if (awareness === null) {
      provider.destroy();
      websocket.destroy();
      doc.destroy();
      throw new Error("Dispatch document provider did not create awareness.");
    }
    sync = startPendingSync({
      doc,
      onChange: callbacks.onPending,
      remoteOrigin: provider,
      store: openPendingEdits(artifactId, doc.clientID),
    });
    void websocket.connect();
    return {
      awareness,
      destroy() {
        if (destroyed) {
          return;
        }
        destroyed = true;
        sync?.destroy();
        provider.destroy();
        websocket.destroy();
        doc.destroy();
      },
      doc,
    };
  };
}
