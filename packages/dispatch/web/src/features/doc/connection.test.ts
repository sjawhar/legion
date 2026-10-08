import { expect, test } from "bun:test";

import {
  colorForLogin,
  connectionLabel,
  isSchemaReadOnly,
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
  expect(connectionLabel("offline", retained)).toBe(
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
  expect(connectionLabel("connected", retained)).toBe("connected");
});
