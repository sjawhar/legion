import { describe, expect, it } from "bun:test";
import { jsonErrorRecoveryHook, repairJson } from "../json-error-recovery";

function makeInput(tool: string, sessionID = "s-1") {
  return { tool, sessionID, callID: "c-1" };
}

function makeOutput(args: Record<string, unknown>) {
  return { args };
}

/** Assert result is non-null, parse as JSON, and return. */
function expectRepaired(input: string): unknown {
  const result = repairJson(input);
  expect(result).not.toBeNull();
  return JSON.parse(result as string);
}

describe("repairJson", () => {
  describe("trailing commas", () => {
    it("removes trailing comma in object", () => {
      expect(expectRepaired('{"a": 1,}')).toEqual({ a: 1 });
    });

    it("removes trailing comma in array", () => {
      expect(expectRepaired("[1, 2, 3,]")).toEqual([1, 2, 3]);
    });

    it("removes multiple trailing commas in nested structures", () => {
      expect(expectRepaired('{"a": [1, 2,], "b": {"c": 3,},}')).toEqual({
        a: [1, 2],
        b: { c: 3 },
      });
    });
  });

  describe("single quotes", () => {
    it("converts single-quoted keys and values to double quotes", () => {
      expect(expectRepaired("{'key': 'val'}")).toEqual({ key: "val" });
    });

    it("handles mixed single and double quotes", () => {
      expect(expectRepaired("{'key': \"val\"}")).toEqual({ key: "val" });
    });

    it("preserves single quotes inside double-quoted strings", () => {
      expect(expectRepaired('{"key": "it\'s a value"}')).toEqual({ key: "it's a value" });
    });
  });

  describe("unquoted keys", () => {
    it("quotes unquoted keys", () => {
      expect(expectRepaired('{key: "val"}')).toEqual({ key: "val" });
    });

    it("quotes multiple unquoted keys", () => {
      expect(expectRepaired('{name: "test", count: 42}')).toEqual({ name: "test", count: 42 });
    });

    it("handles underscored keys", () => {
      expect(expectRepaired('{my_key: "val"}')).toEqual({ my_key: "val" });
    });
  });

  describe("combined malformations", () => {
    it("handles unquoted keys with trailing comma", () => {
      expect(expectRepaired('{key: "val",}')).toEqual({ key: "val" });
    });

    it("handles single quotes with trailing comma", () => {
      expect(expectRepaired("{'key': 'val',}")).toEqual({ key: "val" });
    });
  });

  describe("valid JSON passthrough", () => {
    it("returns valid JSON unchanged", () => {
      expect(expectRepaired('{"key": "val"}')).toEqual({ key: "val" });
    });

    it("returns valid array JSON unchanged", () => {
      expect(expectRepaired("[1, 2, 3]")).toEqual([1, 2, 3]);
    });
  });

  describe("unrecoverable input", () => {
    it("returns null for completely unparseable garbage", () => {
      expect(repairJson("not json at all")).toBeNull();
    });

    it("returns null for partial JSON that can't be fixed", () => {
      expect(repairJson("{key: }")).toBeNull();
    });

    it("returns null for empty string", () => {
      expect(repairJson("")).toBeNull();
    });
  });
});

describe("jsonErrorRecoveryHook", () => {
  describe("repairs JSON string args", () => {
    it("repairs a malformed JSON string in args", () => {
      const output = makeOutput({ content: "{'key': 'val'}" });
      jsonErrorRecoveryHook(makeInput("write"), output);
      expect(output.args.content).toBe('{"key": "val"}');
    });

    it("repairs trailing comma in JSON arg", () => {
      const output = makeOutput({ content: '{"a": 1,}' });
      jsonErrorRecoveryHook(makeInput("write"), output);
      expect(output.args.content).toBe('{"a": 1}');
    });

    it("repairs unquoted keys in JSON arg", () => {
      const output = makeOutput({ content: '{key: "val"}' });
      jsonErrorRecoveryHook(makeInput("write"), output);
      expect(output.args.content).toBe('{"key": "val"}');
    });
  });

  describe("passthrough behavior", () => {
    it("does not modify valid JSON strings", () => {
      const output = makeOutput({ content: '{"key": "val"}' });
      jsonErrorRecoveryHook(makeInput("write"), output);
      expect(output.args.content).toBe('{"key": "val"}');
    });

    it("does not modify non-JSON string args", () => {
      const output = makeOutput({ command: "ls -la" });
      jsonErrorRecoveryHook(makeInput("bash"), output);
      expect(output.args.command).toBe("ls -la");
    });

    it("does not modify non-string args", () => {
      const output = makeOutput({ count: 42, flag: true });
      jsonErrorRecoveryHook(makeInput("bash"), output);
      expect(output.args.count).toBe(42);
      expect(output.args.flag).toBe(true);
    });

    it("does not modify args that are not objects", () => {
      const output = { args: "not an object" as unknown as Record<string, unknown> };
      jsonErrorRecoveryHook(makeInput("bash"), output);
      expect(output.args as unknown).toBe("not an object");
    });
  });

  describe("unrecoverable JSON propagation", () => {
    it("does not modify unrecoverable malformed JSON", () => {
      const output = makeOutput({ content: "totally not json {{{" });
      jsonErrorRecoveryHook(makeInput("write"), output);
      expect(output.args.content).toBe("totally not json {{{");
    });
  });

  describe("multi-arg repair", () => {
    it("repairs multiple JSON args in same output", () => {
      const output = makeOutput({ a: "{'k': 'v'}", b: "{x: 1,}" });
      jsonErrorRecoveryHook(makeInput("write"), output);
      expect(output.args.a).toBe('{"k": "v"}');
      expect(output.args.b).toBe('{"x": 1}');
    });
  });

  describe("only repairs JSON-like strings", () => {
    it("does not attempt repair on plain text strings", () => {
      const output = makeOutput({ description: "Hello world" });
      jsonErrorRecoveryHook(makeInput("write"), output);
      expect(output.args.description).toBe("Hello world");
    });

    it("does not attempt repair on strings without braces or brackets", () => {
      const output = makeOutput({ path: "/home/user/file.json" });
      jsonErrorRecoveryHook(makeInput("read"), output);
      expect(output.args.path).toBe("/home/user/file.json");
    });
  });
});
