import { expect, jest, spyOn, test } from "bun:test";

import {
  colorForLogin,
  connectionLabel,
  type DocumentConnection,
  isSchemaReadOnly,
  loadDocumentTransport,
  pendingNotice,
  wsUrl,
} from "./connection";

test("wsUrl builds an encoded same-origin document websocket URL", () => {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  expect(wsUrl("artifact / one")).toBe(
    `${protocol}//${window.location.host}/ws/doc/artifact%20%2F%20one`
  );
});

test("colorForLogin assigns each login a stable, valid CSS presence color", () => {
  for (const login of ["alice", "bob"]) {
    const color = colorForLogin(login);
    expect(color).toBe(colorForLogin(login));
    expect(CSS.supports("color", color)).toBe(true);
  }
});

test("a read-only Hocuspocus admission signals a schema reload", () => {
  expect(isSchemaReadOnly("readonly")).toBe(true);
  expect(isSchemaReadOnly("read-write")).toBe(false);
  expect(isSchemaReadOnly(undefined)).toBe(false);
});

test("pending document labels describe retained, read-only, rebuilt, and unretained edits", () => {
  const retained = { count: 2, readOnly: false, rebuiltAt: undefined, stored: true };
  expect(connectionLabel("offline", pendingNotice("offline", retained))).toBe(
    "offline · 2 edits saved in this browser, not sent yet."
  );
  expect(pendingNotice("connected", { ...retained, readOnly: true })).toBe(
    "2 edits saved in this browser can't be sent: this document is read-only."
  );
  expect(pendingNotice("connecting", { ...retained, stored: false })).toBe(
    "Edits typed now will not survive a reload."
  );
  expect(pendingNotice("connected", { ...retained, rebuiltAt: 0 })).toBe(
    `Edits saved in this browser at ${new Date(0).toLocaleString()} could not be applied: the document was rebuilt since.`
  );
  expect(pendingNotice("connected", retained)).toBeUndefined();
  expect(connectionLabel("connected", pendingNotice("connected", retained))).toBe("connected");
  expect(pendingNotice("offline", { ...retained, count: 1 })).toBe(
    "1 edit saved in this browser, not sent yet."
  );
});

/** A document socket the test plays the server for: it opens, speaks and closes when told. */
class ServerSocket extends EventTarget {
  static made: ServerSocket[] = [];
  binaryType = "blob";
  readyState = 0;

  constructor(readonly url: string) {
    super();
    ServerSocket.made.push(this);
  }

  open(): void {
    this.readyState = 1;
    this.dispatchEvent(new Event("open"));
  }

  /** The server admits the document read-write: Hocuspocus's Auth message, Authenticated, built
   *  by hand (name, message type 2, auth type 2, scope) so the bytes never come from the code
   *  under test. Any answer ends the provider's connection attempt as a success. */
  admit(name: string): void {
    const text = (value: string) => {
      const bytes = new TextEncoder().encode(value);
      return [bytes.length, ...bytes];
    };
    const data = Uint8Array.from([...text(name), 2, 2, ...text("read-write")]);
    this.dispatchEvent(new MessageEvent("message", { data: data.buffer }));
  }

  /** The server or the network ends the connection. */
  drop(code: number): void {
    this.readyState = 3;
    this.dispatchEvent(Object.assign(new Event("close"), { code, reason: "" }));
  }

  close(): void {
    this.readyState = 3;
  }

  send(): void {}
}

/** Lets the provider's promise chain (its retry's wait, then its attempt) run. */
async function settle(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await Promise.resolve();
}

test("a document's first connect opens at once, and each reconnect after a server close waits a fresh draw of the 2 s spread", async () => {
  const realSocket = globalThis.WebSocket;
  globalThis.WebSocket = ServerSocket as unknown as typeof WebSocket;
  ServerSocket.made = [];
  const random = spyOn(Math, "random").mockReturnValue(0.999);
  const artifact = "connection-test-reconnect";
  let connection: DocumentConnection | undefined;
  try {
    const connect = await loadDocumentTransport();
    connection = connect(artifact, {
      onAdmission() {},
      onOutsideSchema() {},
      onPending() {},
      onStatus() {},
      onSynced() {},
      schemaVersion: 1,
    });
    // The page's first connect waits nothing, whatever the draw would be.
    expect(ServerSocket.made).toHaveLength(1);

    jest.useFakeTimers();
    random.mockReturnValue(0.5);
    const first = ServerSocket.made[0] as ServerSocket;
    first.open();
    first.admit(artifact);
    // Dispatch restarts: the server closes the socket. The reconnect waits 0.5 of 2 s.
    first.drop(1012);
    await settle();
    expect(ServerSocket.made).toHaveLength(1);
    jest.advanceTimersByTime(999);
    await settle();
    expect(ServerSocket.made).toHaveLength(1);
    jest.advanceTimersByTime(1);
    await settle();
    expect(ServerSocket.made).toHaveLength(2);

    // The next reconnect draws again: 0.25 of 2 s.
    random.mockReturnValue(0.25);
    const second = ServerSocket.made[1] as ServerSocket;
    second.open();
    second.admit(artifact);
    second.drop(1006);
    await settle();
    jest.advanceTimersByTime(499);
    await settle();
    expect(ServerSocket.made).toHaveLength(2);
    jest.advanceTimersByTime(1);
    await settle();
    expect(ServerSocket.made).toHaveLength(3);
  } finally {
    connection?.destroy();
    jest.useRealTimers();
    random.mockRestore();
    globalThis.WebSocket = realSocket;
  }
});
