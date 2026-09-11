import { expect, test } from "bun:test";

import { editorAttributes } from "./editor";

test("editorAttributes expose the editor as a multiline textbox", () => {
  expect(editorAttributes).toEqual({
    "aria-label": "Document editor",
    "aria-multiline": "true",
    role: "textbox",
  });
});
