import type { HocuspocusProvider } from "@hocuspocus/provider";
import {
  DOCUMENT_BUSY_CLOSE_CODE,
  DOCUMENT_NEEDS_REBUILD_CLOSE_REASON,
  DOCUMENT_SCHEMA_CLOSE_CODE,
  DOCUMENT_TOO_LARGE_CLOSE_CODE,
  DOCUMENT_TOO_TANGLED_CLOSE_REASON,
} from "@legion/contracts";
import type { Awareness } from "y-protocols/awareness";
import type { Doc } from "yjs";

import { reconnectDelayMs } from "../../api/live-socket";
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

/**
 * The sentence the editor shows for an edit the server refused with
 * `DOCUMENT_TOO_LARGE_CLOSE_CODE`, by the close's reason: an edit the server could not merge into
 * the document quickly enough (`DOCUMENT_TOO_TANGLED_CLOSE_REASON`), one the document cannot take
 * until it is rebuilt (`DOCUMENT_NEEDS_REBUILD_CLOSE_REASON`: its edit history has used up what the
 * server lets it scan, or this browser held a history a rebuild replaced), or one that would make
 * the document too large.
 */
export function refusedEditNotice(reason: string): string {
  switch (reason) {
    case DOCUMENT_TOO_TANGLED_CLOSE_REASON:
      return "Your last edit was not saved: Dispatch could not merge it into this document quickly enough. Your other edits are kept.";
    case DOCUMENT_NEEDS_REBUILD_CLOSE_REASON:
      return "This document has been edited so much that it must be rebuilt before it can take this edit. Your other edits are kept.";
    default:
      return "Your last edit was not saved: it would make this document larger than a Dispatch document may be. Your other edits are kept.";
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
  /**
   * Fires when the server refuses the socket because the stored document is outside the Proof
   * schema. The connection has stopped reconnecting; a fresh read decides what comes next.
   */
  onOutsideSchema(): void;
  /**
   * Fires when the server refused this browser's edit with `DOCUMENT_TOO_LARGE_CLOSE_CODE`, with
   * the close's reason: the edit would take the document past its size bound, the server could
   * not merge it quickly enough (`DOCUMENT_TOO_TANGLED_CLOSE_REASON`), or the document must be
   * rebuilt before it can take it (`DOCUMENT_NEEDS_REBUILD_CLOSE_REASON`). The connection has
   * stopped reconnecting and the refused edit's saved row is gone; the refused text is still in
   * this connection's document, so the host rebuilds it from a fresh read rather than let a
   * reconnect send it again.
   */
  onTooLarge(reason: string): void;
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
    // Whether any admission has been handed to the pending sync: the first one's restore ends in
    // the host's `onSynced`.
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
        this.addEventListener("open", () => {
          // The provider waits `initialDelay` before the first try of every `connect()`, and
          // calls `connect()` again whenever a socket the server had answered closes: the
          // server's close (a restart or roll closes every tab's socket at the same moment) and
          // the provider's own silent-socket check alike. So the page's first connect waits
          // nothing, and once a socket has opened, the reconnect after it waits a fresh draw of
          // the spread. The provider refuses a fractional delay. Tries that fail inside one
          // `connect()` keep the provider's own jittered backoff.
          websocket.setConfiguration({ initialDelay: Math.floor(reconnectDelayMs(0)) });
          sync?.socketOpened();
        });
      }

      override send(data: Parameters<WebSocket["send"]>[0]): void {
        super.send(data);
        if (data instanceof Uint8Array) {
          sync?.frameWritten(data);
        }
      }
    };
    // Nothing here ends a socket that waits for its room. The server answers the upgrade only
    // once it has admitted the browser and loaded the document, which a burst has kept a browser
    // waiting 21.4 s for on the copy bound alone (doc.e2e.ts holds one 25 s). Until the first
    // frame arrives the provider is `connecting`: its `messageReconnectTimeout` check (30 s) skips
    // a socket that is not connected or has received nothing, and its retry has no `timeout`
    // (@hocuspocus/provider 2.15.3, dist/hocuspocus-provider.esm.js:1676,1692,1903-1914). Any
    // timeout added to this path must outlast that wait, or the reconnect queues the browser again.
    const websocket = new HocuspocusProviderWebsocket({
      WebSocketPolyfill,
      connect: false,
      parameters: { schema_version: callbacks.schemaVersion },
      url: wsUrl(artifactId),
    });
    // Every admission's restore waits for its own socket's authentication and SyncStep2 alike,
    // whichever comes last; Dispatch's server queues its SyncStep2 before it reads the socket's
    // token, so the authentication does. A writable admission restores the rows other tabs stored,
    // judging each against this document; before the reconnect's sync applies, the document lacks
    // what the server took meanwhile, so a row that builds on it would look as if it were typed
    // against a rebuilt history and be dropped. The provider reports each once per socket: `synced`
    // only as `isSynced` changes, which a close resets, and `authenticated` once per Authenticated
    // frame, which the server sends once in answer to the one token each socket sends. So this
    // hands the pending sync one admission per socket, and `synced()` takes a repeated one as a
    // no-op all the same. Each admission's restore is kept as `restored`, which a refusal of this
    // browser's edit waits for before it drops the refused row (`dropRefused`).
    const notifyAdmitted = () => {
      if (!provider.isSynced || !provider.isAuthenticated) {
        return;
      }
      const restoring = Promise.resolve(sync?.synced()).catch((error: unknown) => {
        console.error("Could not restore this document's pending browser edits", error);
      });
      restored = restoring;
      if (announced) {
        return;
      }
      announced = true;
      void restoring.then(() => {
        if (!destroyed) {
          callbacks.onSynced();
        }
      });
    };
    provider = new HocuspocusProvider({
      document: doc,
      name: artifactId,
      onAuthenticated: () => {
        const readOnly = isSchemaReadOnly(provider.authorizedScope);
        // Whether the admission is read-only decides from here on whether the editor takes typing
        // and whether the pending sync stores a local update as a person's edit.
        sync?.authenticated(readOnly);
        callbacks.onAdmission(readOnly);
        notifyAdmitted();
      },
      onClose: ({ event }) => {
        // Hocuspocus answers every other close by reconnecting, which is what the transient
        // `DOCUMENT_BUSY_CLOSE_CODE` wants: the server is out of room for open documents, and a
        // later attempt finds what it has freed, so the edits it refused wait to go with it. The
        // other two are the server's decision about the document itself, so the provider stops:
        // a document outside the Proof schema the host reads again, and an edit past the
        // document's size bound, one the server could not merge quickly enough, or one the
        // document cannot take until it is rebuilt, is dropped before the host rebuilds the
        // document without it.
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
                callbacks.onTooLarge(event.reason);
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
      // The provider sets `isSynced` before it reports a change: `true` once the socket's
      // SyncStep2 has been applied, `false` as the socket closes, before `onClose` above runs.
      onSynced: notifyAdmitted,
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
