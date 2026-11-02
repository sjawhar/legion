import { describe, expect, test } from "bun:test";

import type { Anchor, Comment } from "../../api/types";
import {
  boxFromDrag,
  boxStyle,
  fileBoxNumbers,
  fileThreadLabel,
  isFileAnchor,
} from "./picture-box";

const picture = { left: 100, top: 50, width: 400, height: 200 };

describe("boxFromDrag", () => {
  test("gives the dragged part of the picture as fractions of its width and height", () => {
    expect(boxFromDrag({ x: 140, y: 70 }, { x: 260, y: 120 }, picture)).toEqual({
      left: 0.1,
      top: 0.1,
      width: 0.3,
      height: 0.25,
    });
  });

  test("gives the same box whichever corner the drag starts from", () => {
    const forward = boxFromDrag({ x: 140, y: 70 }, { x: 260, y: 120 }, picture);
    expect(boxFromDrag({ x: 260, y: 120 }, { x: 140, y: 70 }, picture)).toEqual(forward);
    expect(boxFromDrag({ x: 260, y: 70 }, { x: 140, y: 120 }, picture)).toEqual(forward);
  });

  test("stops a drag that leaves the picture at its edge", () => {
    expect(boxFromDrag({ x: 300, y: 150 }, { x: 900, y: 900 }, picture)).toEqual({
      left: 0.5,
      top: 0.5,
      width: 0.5,
      height: 0.5,
    });
    expect(boxFromDrag({ x: 140, y: 70 }, { x: -50, y: -50 }, picture)).toEqual({
      left: 0,
      top: 0,
      width: 0.1,
      height: 0.1,
    });
  });

  test("a drag shorter than a few pixels either way draws no box", () => {
    expect(boxFromDrag({ x: 140, y: 70 }, { x: 142, y: 170 }, picture)).toBeUndefined();
    expect(boxFromDrag({ x: 140, y: 70 }, { x: 240, y: 71 }, picture)).toBeUndefined();
  });

  test("keeps its far sides inside the picture after rounding", () => {
    const box = boxFromDrag({ x: 100.3, y: 50.7 }, { x: 499.9, y: 249.99 }, picture);
    expect(box).toBeDefined();
    if (box === undefined) return;
    expect(box.left + box.width).toBeLessThanOrEqual(1 + 1e-9);
    expect(box.top + box.height).toBeLessThanOrEqual(1 + 1e-9);
  });
});

test("boxStyle places the box by percentages of the picture", () => {
  expect(boxStyle({ left: 0.1, top: 0.25, width: 0.3, height: 0.5 })).toEqual({
    left: "10%",
    top: "25%",
    width: "30%",
    height: "50%",
  });
});

function fileComment(
  id: string,
  createdAt: string,
  anchor: Partial<Anchor> | null,
  extra: Partial<Comment> = {}
): Comment {
  return {
    id,
    issue_key: "CORE-1",
    author: { kind: "user", id: "alice" },
    body: id,
    anchor:
      anchor === null
        ? null
        : {
            kind: "file",
            artifact_id: "chart",
            block_id: null,
            mark_id: id,
            version: 1,
            quote: "",
            orphaned: false,
            ...anchor,
          },
    reply_to: null,
    ask_id: null,
    turn: null,
    resolved: false,
    resolved_by: null,
    resolved_at: null,
    edited_at: null,
    suggestion: null,
    created_at: createdAt,
    mentions: [],
    deliveries: [],
    ...extra,
  };
}

const box = { left: 0.1, top: 0.1, width: 0.2, height: 0.2 };

test("fileBoxNumbers numbers a file's box threads by when they were written, resolved ones too", () => {
  const comments = [
    fileComment("third", "2026-11-01T10:00:02Z", { box }),
    fileComment("first", "2026-11-01T10:00:00Z", { box, version: 2 }),
    fileComment("whole", "2026-11-01T10:00:01Z", {}),
    fileComment("second", "2026-11-01T10:00:01.5Z", { box }, { resolved: true }),
    fileComment("reply", "2026-11-01T10:00:03Z", null, { reply_to: "first" }),
    fileComment("other-file", "2026-11-01T09:00:00Z", { box, artifact_id: "table" }),
    fileComment("text", "2026-11-01T09:00:00Z", {
      kind: undefined,
      quote: "words",
      box: undefined,
    }),
  ];
  expect([...fileBoxNumbers(comments, "chart")]).toEqual([
    ["first", 1],
    ["second", 2],
    ["third", 3],
  ]);
});

test("isFileAnchor tells a file anchor from a passage's", () => {
  expect(isFileAnchor(fileComment("a", "2026-11-01T10:00:00Z", {}).anchor)).toBe(true);
  expect(
    isFileAnchor(fileComment("b", "2026-11-01T10:00:00Z", { kind: undefined, quote: "x" }).anchor)
  ).toBe(false);
  expect(isFileAnchor(null)).toBe(false);
});

test("fileThreadLabel names the box and its version, or the whole file", () => {
  const anchor = fileComment("a", "2026-11-01T10:00:00Z", { box, version: 2 }).anchor;
  const whole = fileComment("b", "2026-11-01T10:00:00Z", { version: 3 }).anchor;
  if (anchor === null || whole === null) throw new Error("fixture has no anchor");
  expect(fileThreadLabel(anchor, 4)).toBe("Box 4 · version 2");
  expect(fileThreadLabel(anchor, undefined)).toBe("Box · version 2");
  expect(fileThreadLabel(whole, undefined)).toBe("Whole file · version 3");
});
