import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { appKeymap } from "../shell/keymap";
import { CopyMarkdownButton } from "./CopyMarkdownButton";

const MARKDOWN = "# Design\n\nUse **Postgres**.\n";

let restoreClipboard = () => {};
let writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
let popScope = () => {};

beforeEach(() => {
  const original = navigator.clipboard;
  writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  restoreClipboard = () =>
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: original });
  // The document's own page pushes this scope (`ProofDocument`); the button only registers into it.
  popScope = appKeymap.pushScope("document");
});

afterEach(() => {
  popScope();
  restoreClipboard();
});

/** The palette's Copy Markdown row, which is never offered twice. */
function copyMarkdownRow() {
  const rows = appKeymap.actions().filter((action) => action.label === "Copy Markdown");
  expect(rows.length).toBeLessThanOrEqual(1);
  return rows[0];
}

test("Copy Markdown reads the Markdown when pressed, copies it, and confirms as the link button does", async () => {
  let reads = 0;
  const view = render(
    <CopyMarkdownButton
      read={async () => {
        reads += 1;
        return MARKDOWN;
      }}
    />
  );
  try {
    const button = screen.getByRole("button", { name: "Copy Markdown" });
    expect(button.textContent).toBe("Markdown");
    expect(reads).toBe(0);
    fireEvent.click(button);
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(MARKDOWN));
    expect(reads).toBe(1);
    expect(await screen.findByText("Copied", { exact: true })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("the palette offers Copy Markdown while the button is on the page, and copies through it", async () => {
  const view = render(<CopyMarkdownButton read={async () => MARKDOWN} />);
  try {
    const row = copyMarkdownRow();
    if (row === undefined) throw new Error("no Copy Markdown row in the palette");
    expect(row).toMatchObject({ keys: [], scope: "document" });
    act(() => row.run());
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(MARKDOWN));
    expect(await screen.findByText("Copied", { exact: true })).toBeTruthy();
  } finally {
    view.unmount();
  }
  expect(copyMarkdownRow()).toBeUndefined();
});

// The browser's own clipboard item, whose text is still being read: what keeps the click's
// permission over the read in Safari.
test("Copy Markdown hands the clipboard an item that holds the read, where the browser takes one", async () => {
  const globals = globalThis as { ClipboardItem?: unknown };
  const originalItem = globals.ClipboardItem;
  class FakeClipboardItem {
    constructor(readonly items: Record<string, Promise<Blob>>) {}
    getType(type: string): Promise<Blob> {
      const item = this.items[type];
      if (item === undefined) throw new Error(`no ${type}`);
      return item;
    }
  }
  globals.ClipboardItem = FakeClipboardItem;
  const written: string[] = [];
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: {
      write: async (items: FakeClipboardItem[]) => {
        for (const item of items) written.push(await (await item.getType("text/plain")).text());
      },
      writeText,
    },
  });
  let finishRead: (markdown: string) => void = () => {};
  const view = render(
    <CopyMarkdownButton read={() => new Promise((resolve) => (finishRead = resolve))} />
  );
  try {
    fireEvent.click(screen.getByRole("button", { name: "Copy Markdown" }));
    finishRead(MARKDOWN);
    await waitFor(() => expect(written).toEqual([MARKDOWN]));
    expect(writeText).not.toHaveBeenCalled();
    expect(await screen.findByText("Copied", { exact: true })).toBeTruthy();
  } finally {
    view.unmount();
    globals.ClipboardItem = originalItem;
  }
});

test("a failed copy says so without printing the document", async () => {
  const originalExecCommand = document.execCommand;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (_command: string) => false,
  });
  const view = render(<CopyMarkdownButton read={async () => MARKDOWN} />);
  try {
    fireEvent.click(screen.getByRole("button", { name: "Copy Markdown" }));
    const failed = await screen.findByText("Copy failed - try again", { exact: true });
    expect(failed.getAttribute("aria-live")).toBe("polite");
    expect(view.container.textContent).not.toContain("Postgres");
  } finally {
    view.unmount();
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
  }
});

test("a read that fails is a failed copy, and nothing reaches the clipboard", async () => {
  const view = render(
    <CopyMarkdownButton read={() => Promise.reject(new Error("Dispatch is unreachable"))} />
  );
  try {
    fireEvent.click(screen.getByRole("button", { name: "Copy Markdown" }));
    expect(await screen.findByText("Copy failed - try again", { exact: true })).toBeTruthy();
    expect(writeText).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});
