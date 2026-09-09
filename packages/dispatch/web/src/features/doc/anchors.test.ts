import { expect, test } from "bun:test";
import { EditorState } from "@codemirror/state";

import {
  anchorDecorationState,
  type MappedAnchor,
  resolveMappedAnchor,
  setAnchorDecorations,
} from "./anchors";

const brown: MappedAnchor = {
  from: 10,
  orphaned: false,
  quote: "brown",
  to: 15,
};

test("an anchor shifts after insertion before its range", () => {
  const resolved = resolveMappedAnchor(brown, "Note: The quick brown fox", (position) =>
    position < 10 ? position : position + 6
  );

  expect(resolved).toEqual({ ...brown, from: 16, to: 21 });
});

test("an anchor becomes orphaned when its quoted range is deleted", () => {
  const fox: MappedAnchor = { from: 16, orphaned: false, quote: "fox", to: 19 };
  const resolved = resolveMappedAnchor(fox, "The quick brown ", () => 16);

  expect(resolved).toEqual({ ...fox, orphaned: true });
});

test("an anchor re-resolves to the nearest identical quote", () => {
  const secondRed: MappedAnchor = { from: 4, orphaned: false, quote: "red", to: 7 };
  const resolved = resolveMappedAnchor(secondRed, "red text red", (position) => position + 2);

  expect(resolved).toEqual({ ...secondRed, from: 9, to: 12 });
});

test("anchor decorations survive a document transaction", () => {
  let state = EditorState.create({
    doc: "The quick brown fox",
    extensions: [anchorDecorationState],
  });
  state = state.update({
    effects: setAnchorDecorations.of([{ anchor: brown, id: "comment-1", selected: false }]),
  }).state;

  state = state.update({ changes: { from: 0, insert: "Note: " } }).state;

  const anchors = state.field(anchorDecorationState);
  expect(anchors.anchors.get("comment-1")).toMatchObject({ from: 16, quote: "brown", to: 21 });
  expect(anchors.decorations.size).toBe(1);
});
