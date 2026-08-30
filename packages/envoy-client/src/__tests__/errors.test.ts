import { describe, expect, test } from "bun:test";
import { messageFor } from "../errors";

describe("messageFor", () => {
  test("returns the message from Error values", () => {
    expect(messageFor(new Error("network unavailable"))).toBe("network unavailable");
  });

  test("coerces non-Error thrown values for diagnostic output", () => {
    expect(messageFor("plain failure")).toBe("plain failure");
    expect(messageFor(401)).toBe("401");
    expect(messageFor(null)).toBe("null");
  });
});
