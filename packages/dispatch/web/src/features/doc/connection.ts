import type { HocuspocusProvider } from "@hocuspocus/provider";
import {
  DOCUMENT_BUSY_CLOSE_CODE,
  DOCUMENT_SCHEMA_CLOSE_CODE,
  DOCUMENT_TOO_LARGE_CLOSE_CODE,
} from "@legion/contracts";
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
  /**
   * Fires when the server refused this browser's edit because it would take the document past its
   * size bound (`DOCUMENT_TOO_LARGE_CLOSE_CODE`). The connection has stopped reconnecting and the
   * refused edit's saved row is gone; the refused text is still in this connection's document, so
   * the host rebuilds it from a fresh read rather than let a reconnect send it again.
   */
  onTooLarge(): void;
  /**
   * Fires `true` when the server closed the socket because the memory it keeps for open documents
   * is full (`DOCUMENT_BUSY_CLOSE_CODE`), and `false` once it applies a frame from this browser
   * again. The provider keeps reconnecting by itself, and the edits it could not send stay in this
   * document and in the browser's saved rows until a socket takes them.
   */
  onBusy(waiting: boolean): void;
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
    { readSyncStatus, startPendingSync },
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
    let synced = false;
    let authenticated = false;
    let announced = false;
    // This admission's restore of the browser's saved rows; a refused edit is dropped after it.
    let restored: Promise<void> = Promise.resolve();
    // The server last closed a socket for want of room, and has applied nothing from this browser
    // since.
    let busy = false;
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
    const notifySynced = () => {
      if (synced && authenticated && !announced) {
        announced = true;
        restored = Promise.resolve(
          sync?.firstSync(isSchemaReadOnly(provider.authorizedScope))
        ).catch((error: unknown) => {
          console.error("Could not restore this document's pending browser edits", error);
        });
        void restored.then(() => {
          if (!destroyed) {
            callbacks.onSynced();
          }
        });
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
      onClose: ({ event }) => {
        // Hocuspocus answers every other close by reconnecting, which is what the transient
        // `DOCUMENT_BUSY_CLOSE_CODE` wants: the server is out of room for open documents, and a
        // later attempt finds what it has freed, so the edits it refused wait to go with it. The
        // other two are the server's decision about the document itself, so the provider stops:
        // a document outside the Proof schema the host reads again, and an edit past the
        // document's size bound is dropped before the host rebuilds the document without it.
        switch (event.code) {
          case DOCUMENT_SCHEMA_CLOSE_CODE:
            provider.disconnect();
            callbacks.onOutsideSchema();
            break;
          case DOCUMENT_TOO_LARGE_CLOSE_CODE:
            // The refused frame is taken now: the provider's own close handler, which runs first,
            // has already started a reconnect that this disconnect ends.
            void Promise.resolve(sync?.dropRefused(restored)).then(() => {
              if (!destroyed) {
                callbacks.onTooLarge();
              }
            });
            provider.disconnect();
            break;
          case DOCUMENT_BUSY_CLOSE_CODE:
            if (!busy) {
              busy = true;
              callbacks.onBusy(true);
            }
            break;
        }
      },
      onMessage: ({ event }) => {
        if (!(event.data instanceof ArrayBuffer)) {
          return;
        }
        const frame = new Uint8Array(event.data);
        if (busy) {
          let applied = false;
          try {
            applied = readSyncStatus(frame) === true;
          } catch {
            // frameReceived reports a frame that does not parse.
          }
          if (applied) {
            busy = false;
            callbacks.onBusy(false);
          }
        }
        void sync?.frameReceived(frame);
      },
      onStatus: ({ status }) => callbacks.onStatus(status === "disconnected" ? "offline" : status),
      onSynced: ({ state }) => {
        if (state) {
          synced = true;
          notifySynced();
        }
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
