import { expect, test } from "bun:test";

import { colorForLogin, wsUrl } from "./connection";

test("wsUrl builds an encoded same-origin document websocket URL", () => {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  expect(wsUrl("artifact / one")).toBe(
    `${protocol}//${window.location.host}/ws/doc/artifact%20%2F%20one`
  );
});

test("colorForLogin assigns each login a stable presence color", () => {
  expect(colorForLogin("alice")).toBe("#0284c7");
  expect(colorForLogin("bob")).toBe("#7c3aed");
  expect(colorForLogin("alice")).toBe(colorForLogin("alice"));
});
