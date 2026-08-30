import { describe, expect, test } from "bun:test";
import { isJsonRpcRequest } from "../dispatch-mcp-shim";

describe("isJsonRpcRequest", () => {
  test("accepts JSON-RPC requests and notifications the bridge can forward", () => {
    expect(isJsonRpcRequest({ jsonrpc: "2.0", id: 1, method: "initialize" })).toBe(true);
    expect(isJsonRpcRequest({ jsonrpc: "2.0", id: "call-1", method: "tools/call" })).toBe(true);
    expect(
      isJsonRpcRequest({ jsonrpc: "2.0", id: null, method: "notifications/initialized" })
    ).toBe(true);
    expect(isJsonRpcRequest({ jsonrpc: "2.0", method: "notifications/initialized" })).toBe(true);
  });

  test("rejects malformed stdin payloads before they reach the bridge", () => {
    const invalidPayloads = [
      null,
      "not-json-rpc",
      [],
      { jsonrpc: "1.0", method: "tools/list" },
      { jsonrpc: "2.0" },
      { jsonrpc: "2.0", method: 42 },
      { jsonrpc: "2.0", id: false, method: "tools/list" },
      { jsonrpc: "2.0", id: {}, method: "tools/list" },
    ];

    for (const payload of invalidPayloads) {
      expect(isJsonRpcRequest(payload)).toBe(false);
    }
  });
});
