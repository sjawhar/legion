import { expect, test } from "bun:test";
import { DOCUMENT_EDITOR_SCHEMA_VERSION } from "@legion/contracts";
import { waitFor } from "@testing-library/react";

import { installOpenSockets, OpenSocket } from "../../__tests__/open-socket";
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

test("pending document labels describe retained, read-only, rebuilt, stripped, and unretained edits", () => {
  const retained = {
    count: 2,
    readOnly: false,
    rebuiltAt: undefined,
    stored: true,
    strippedAt: undefined,
  };
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
  expect(pendingNotice("connected", { ...retained, strippedAt: 0 })).toBe(
    `Edits saved in this browser at ${new Date(0).toLocaleString()} could not be applied: an older version of Dispatch made them, and they would remove an image's link.`
  );
  expect(pendingNotice("connected", retained)).toBeUndefined();
  expect(connectionLabel("connected", pendingNotice("connected", retained))).toBe("connected");
  expect(pendingNotice("offline", { ...retained, count: 1 })).toBe(
    "1 edit saved in this browser, not sent yet."
  );
});

test("a document connection presents its schema version and the bundle's editor schema", async () => {
  const restoreSockets = installOpenSockets();
  let connection: DocumentConnection | undefined;
  try {
    const connect = await loadDocumentTransport();
    connection = connect("connection-test", {
      onAdmission() {},
      onOutsideSchema() {},
      onPending() {},
      onStatus() {},
      onSynced() {},
      schemaVersion: 7,
    });
    // The Hocuspocus token message, built by hand: the document name, the authentication tag and
    // the token subtype, then the token, each string after its length.
    const encoder = new TextEncoder();
    const name = encoder.encode("connection-test");
    const token = encoder.encode(`7:${DOCUMENT_EDITOR_SCHEMA_VERSION}`);
    const authentication = Uint8Array.from([name.length, ...name, 2, 0, token.length, ...token]);
    await waitFor(() => expect(OpenSocket.opened[0]?.sent).toContainEqual(authentication));

    const query = new URL((OpenSocket.opened[0] as OpenSocket).url).searchParams;
    expect(query.get("schema_version")).toBe("7");
    expect(query.get("editor_schema")).toBe(String(DOCUMENT_EDITOR_SCHEMA_VERSION));
  } finally {
    connection?.destroy();
    restoreSockets();
  }
});
