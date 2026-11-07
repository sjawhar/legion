import { expect, test } from "bun:test";

import { holdSection } from "./pane-placement";

/** A column with something drawn in it, which is where a reader's press lands. */
function column(): { box: HTMLElement; inside: HTMLElement } {
  const box = document.createElement("div");
  const inside = document.createElement("p");
  box.append(inside);
  document.body.append(box);
  return { box, inside };
}

test("a column nobody has touched is still held at its section", () => {
  const { box } = column();
  const hold = holdSection(box);
  try {
    // The document drawing itself is not the reader moving: the placement is applied again for
    // every block that lands above the heading, which is the defect this hold exists for.
    expect(hold.held()).toBe(true);
    expect(hold.held()).toBe(true);
  } finally {
    hold.release();
    box.remove();
  }
});

for (const action of ["pointerdown", "wheel", "touchstart", "keydown"]) {
  test(`${action} in the column is the reader moving on, so the column is let go`, () => {
    const { box, inside } = column();
    const hold = holdSection(box);
    try {
      inside.dispatchEvent(new Event(action, { bubbles: true }));
      expect(hold.held()).toBe(false);
    } finally {
      hold.release();
      box.remove();
    }
  });
}

// Selecting another row is a press in the list beside the column, and the column it opens is
// placed at its section like any other.
test("a press outside the column leaves it held", () => {
  const { box } = column();
  const elsewhere = document.createElement("button");
  document.body.append(elsewhere);
  const hold = holdSection(box);
  try {
    elsewhere.dispatchEvent(new Event("pointerdown", { bubbles: true }));
    expect(hold.held()).toBe(true);
  } finally {
    hold.release();
    box.remove();
    elsewhere.remove();
  }
});

test("an event of another kind leaves the column held", () => {
  const { box, inside } = column();
  const hold = holdSection(box);
  try {
    inside.dispatchEvent(new Event("scroll", { bubbles: true }));
    inside.dispatchEvent(new Event("pointermove", { bubbles: true }));
    expect(hold.held()).toBe(true);
  } finally {
    hold.release();
    box.remove();
  }
});

test("a released hold stays let go, whatever the reader does next", () => {
  const { box, inside } = column();
  const hold = holdSection(box);
  hold.release();

  expect(hold.held()).toBe(false);
  inside.dispatchEvent(new Event("pointerdown", { bubbles: true }));
  expect(hold.held()).toBe(false);
  box.remove();
});
