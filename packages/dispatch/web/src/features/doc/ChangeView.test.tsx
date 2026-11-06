import { expect, spyOn, test } from "bun:test";
import { fireEvent, render, within } from "@testing-library/react";

import { ChangeView } from "./ChangeView";
import * as changeSet from "./change-set";

/**
 * The marks a block carries, in order: `del` for what the earlier version held, `ins` for what
 * the later one gained. Everything else the block holds is unmarked.
 */
function marks(block: Element | null): (string | null)[] {
  return Array.from(block?.querySelectorAll("del, ins") ?? []).map((mark) => mark.textContent);
}

/** Every block the view shows, as its `data-change` and its text, in the order it renders. */
function blocks(container: HTMLElement): [string | null, string | null][] {
  return Array.from(container.querySelectorAll("[data-change]")).map((block) => [
    block.getAttribute("data-change"),
    block.textContent,
  ]);
}

// Every view is unmounted by hand: the suite's preload registers no testing-library cleanup, and
// a view left in the shared document reaches every later test that looks for one.

test("a word change inside a paragraph marks the words alone", () => {
  const view = render(
    <ChangeView
      after="The plan ships behind a canary today."
      before="The plan ships behind a flag today."
    />
  );
  try {
    const changed = within(view.container).getByTestId("change-view").firstElementChild;
    expect(changed?.getAttribute("data-change")).toBe("changed");
    expect(marks(changed)).toEqual(["flag", "canary"]);
    expect(changed?.textContent).toBe("The plan ships behind a flagcanary today.");
  } finally {
    view.unmount();
  }
});

test("a paragraph only the later version holds is added", () => {
  const view = render(
    <ChangeView
      after={"# Plan\n\nOne paragraph.\n\nA second paragraph.\n"}
      before={"# Plan\n\nOne paragraph.\n"}
    />
  );
  try {
    expect(blocks(view.container)).toEqual([
      ["unchanged", "# Plan"],
      ["unchanged", "One paragraph."],
      ["added", "A second paragraph."],
    ]);
    const added = view.container.querySelector('[data-change="added"]');
    expect(added?.querySelector("ins")?.textContent).toBe("A second paragraph.");
  } finally {
    view.unmount();
  }
});

test("a paragraph only the earlier version holds is struck through where it stood", () => {
  const view = render(
    <ChangeView
      after={"First paragraph.\n\nThird paragraph.\n"}
      before={"First paragraph.\n\nSecond paragraph.\n\nThird paragraph.\n"}
    />
  );
  try {
    expect(blocks(view.container)).toEqual([
      ["unchanged", "First paragraph."],
      ["removed", "Second paragraph."],
      ["unchanged", "Third paragraph."],
    ]);
    const removed = view.container.querySelector('[data-change="removed"] del');
    expect(removed?.className).toContain("line-through");
  } finally {
    view.unmount();
  }
});

test("a list item moved inside its list marks the item alone", () => {
  const view = render(
    <ChangeView
      after={"- Beta\n- Gamma\n- Delta\n- Alpha\n"}
      before={"- Alpha\n- Beta\n- Gamma\n- Delta\n"}
    />
  );
  try {
    const list = view.container.querySelector("[data-change]");
    expect(list?.getAttribute("data-change")).toBe("changed");
    expect(blocks(view.container).length).toBe(1);
    expect(marks(list)).toEqual(["Alpha\n- ", "\n- Alpha"]);
    expect(list?.textContent).toContain("Beta\n- Gamma\n- Delta");
  } finally {
    view.unmount();
  }
});

test("a changed table row marks the cell that changed, the table staying one block", () => {
  const header = "| Region | Wave |\n| --- | --- |\n";
  const view = render(
    <ChangeView after={`${header}| West | 2 |\n`} before={`${header}| West | 1 |\n`} />
  );
  try {
    const table = view.container.querySelector("[data-change]");
    expect(table?.getAttribute("data-change")).toBe("changed");
    expect(blocks(view.container).length).toBe(1);
    expect(marks(table)).toEqual(["1", "2"]);
    expect(table?.textContent).toContain("| Region | Wave |");
  } finally {
    view.unmount();
  }
});

test("a run of unchanged blocks folds behind one control, a block of context on each side", () => {
  const body = "\n\nA.\n\nB.\n\nC.\n\nD.\n\n";
  const view = render(
    <ChangeView after={`First changed.${body}Last changed.\n`} before={`First.${body}Last.\n`} />
  );
  try {
    const card = within(view.container).getByTestId("change-view");
    expect(blocks(view.container)).toEqual([
      ["changed", "First changed."],
      ["unchanged", "A."],
      ["unchanged", "D."],
      ["changed", "Last changed."],
    ]);

    const show = within(card).getByRole("button", { name: "Show 2 unchanged blocks" });
    fireEvent.click(show);
    expect(blocks(view.container)).toEqual([
      ["changed", "First changed."],
      ["unchanged", "A."],
      ["unchanged", "B."],
      ["unchanged", "C."],
      ["unchanged", "D."],
      ["changed", "Last changed."],
    ]);

    const hide = within(card).getByRole("button", { name: "Hide 2 unchanged blocks" });
    fireEvent.click(hide);
    expect(blocks(view.container).length).toBe(4);
  } finally {
    view.unmount();
  }
});

test("the heading of the section a change sits in is drawn while the heading itself is folded", () => {
  const body = "# Plan\n\nIntro.\n\n## Rollout\n\nAlpha.\n\nBeta.\n\nGamma.\n\n";
  const view = render(<ChangeView after={`${body}Delta changed.\n`} before={`${body}Delta.\n`} />);
  try {
    const card = within(view.container).getByTestId("change-view");
    expect(within(card).getByTestId("change-section").textContent).toBe("## Rollout");
    expect(blocks(view.container)).toEqual([
      ["unchanged", "Gamma."],
      ["changed", "Delta changed."],
    ]);

    fireEvent.click(within(card).getByRole("button", { name: "Show 5 unchanged blocks" }));
    expect(within(card).queryByTestId("change-section")).toBeNull();
    expect(blocks(view.container).map(([change]) => change)).toEqual([
      "unchanged",
      "unchanged",
      "unchanged",
      "unchanged",
      "unchanged",
      "unchanged",
      "changed",
    ]);
  } finally {
    view.unmount();
  }
});

test("two versions that hold the same text show the document, unmarked and unfolded", () => {
  const document = "# Plan\n\nAlpha.\n\nBeta.\n\nGamma.\n\nDelta.\n";
  const view = render(<ChangeView after={document} before={document} />);
  try {
    const card = within(view.container).getByTestId("change-view");
    expect(blocks(view.container).map(([change]) => change)).toEqual([
      "unchanged",
      "unchanged",
      "unchanged",
      "unchanged",
      "unchanged",
    ]);
    expect(card.querySelector("ins, del")).toBeNull();
    expect(within(card).queryByRole("button")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("opening a fold diffs no changed block the view already diffed", () => {
  const version = (first: string, middle: string, last: string): string =>
    `${first}\n\nA.\n\nB.\n\nC.\n\nD.\n\n${middle}\n\nE.\n\nF.\n\nG.\n\nH.\n\n${last}\n`;
  const changedWords = spyOn(changeSet, "changedWords");
  const view = render(
    <ChangeView
      after={version("First changed.", "Middle changed.", "Last changed.")}
      before={version("First.", "Middle.", "Last.")}
    />
  );
  try {
    const card = within(view.container).getByTestId("change-view");
    // One word diff per changed pair, and none for the blocks both versions hold.
    const diffed = changedWords.mock.calls.length;
    expect(diffed).toBe(3);

    const folds = within(card).getAllByRole("button");
    expect(folds.map((fold) => fold.textContent)).toEqual([
      "Show 2 unchanged blocks",
      "Show 2 unchanged blocks",
    ]);
    fireEvent.click(folds[0]);
    expect(changedWords.mock.calls.length).toBe(diffed);
    fireEvent.click(within(card).getByRole("button", { name: "Show 2 unchanged blocks" }));
    expect(changedWords.mock.calls.length).toBe(diffed);
    expect(blocks(view.container).map(([change]) => change)).toEqual([
      "changed",
      "unchanged",
      "unchanged",
      "unchanged",
      "unchanged",
      "changed",
      "unchanged",
      "unchanged",
      "unchanged",
      "unchanged",
      "changed",
    ]);
  } finally {
    changedWords.mockRestore();
    view.unmount();
  }
});
