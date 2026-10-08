import { expect, test } from "bun:test";
import { toolSuccess } from "./tool-result";

test("toolSuccess puts each picture after the text as the host's image block", () => {
  expect(
    toolSuccess("Picture shot.png: image/png, version 1, 8 bytes.", { session: "s" }, [
      { data: "iVBORw0KGgo=", mimeType: "image/png" },
      { data: "/9j/4A==", mimeType: "image/jpeg" },
    ])
  ).toEqual({
    content: [
      { type: "text", text: "Picture shot.png: image/png, version 1, 8 bytes." },
      { type: "image", data: "iVBORw0KGgo=", mimeType: "image/png" },
      { type: "image", data: "/9j/4A==", mimeType: "image/jpeg" },
    ],
    details: { session: "s" },
  });
  expect(toolSuccess("Posted.").content).toEqual([{ type: "text", text: "Posted." }]);
});
