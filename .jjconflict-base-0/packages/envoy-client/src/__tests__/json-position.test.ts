import { describe, expect, test } from "bun:test";
import { jsonErrorPosition } from "../json-position";

describe("jsonErrorPosition", () => {
  test("JSON has no position", () => {
    for (const text of [
      "{}",
      " [ ] ",
      '{"dispatch": {"enabled": true, "token": "a\\"b"}, "n": [-1.5e3, 0, null, false]}',
      '\n{\n  "envoy": {"natsUrls": ["nats://n:4222"]}\n}\n',
    ]) {
      expect(jsonErrorPosition(text)).toBeUndefined();
    }
  });

  test("names the line and column where the text stops being JSON", () => {
    const cases: [string, string][] = [
      ['{\n  "dispatch": {"token": hunter2secret}\n}', "line 2, column 25"],
      ['{"dispatch": {"token": SUSEED}}', "line 1, column 24"],
      ["{", "line 1, column 2"],
      ['{"a": 1,}', "line 1, column 9"],
      ['{"a": 1} trailing', "line 1, column 10"],
      ['{"a": "line\nbreak"}', "line 1, column 12"],
      ['{"a" 1}', "line 1, column 6"],
      ["", "line 1, column 1"],
      ["[01]", "line 1, column 3"],
    ];
    for (const [text, position] of cases) {
      expect([text, jsonErrorPosition(text)]).toEqual([text, position]);
    }
  });
});
