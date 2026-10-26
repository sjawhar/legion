import { expect, test } from "bun:test";

import { scanEditorReferences } from "./editor-references";

test("the editor's candidates are the bare keys, #ids and dispatch:// addresses the shared grammar reads", () => {
  const text =
    "See CORE-1, #q2 and dispatch://CORE-1/spec; not https://x.test/issues/CORE-2 or xCORE-3.";

  expect(
    scanEditorReferences(text).map(({ end, key, start }) => ({ key, text: text.slice(start, end) }))
  ).toEqual([
    { key: "CORE-1", text: "CORE-1" },
    { key: "#q2", text: "#q2" },
    { key: "dispatch://CORE-1/spec", text: "dispatch://CORE-1/spec" },
  ]);
});
