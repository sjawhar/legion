import { expect, spyOn, test } from "bun:test";
import { render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { CopyRefButton } from "./CopyRefButton";

function withClipboard(
  clipboard: { writeText: (text: string) => Promise<void> } | undefined,
  run: () => Promise<void>
): Promise<void> {
  const original = navigator.clipboard;
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: clipboard });
  return run().finally(() => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: original });
  });
}

test("CopyRefButton copies the dispatch:// reference of its route", async () => {
  const user = userEvent.setup();
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  await withClipboard({ writeText }, async () => {
    const view = render(
      <CopyRefButton
        route={{
          item: { id: "ask 1", kind: "ask" },
          kind: "document",
          project: "CORE",
          slug: "design notes",
        }}
      />
    );
    try {
      const button = view.getByRole("button", {
        name: "Copy reference dispatch://CORE/artifact/design%20notes/ask/ask%201",
      });
      await user.click(button);
      await waitFor(() =>
        expect(writeText).toHaveBeenCalledWith(
          "dispatch://CORE/artifact/design%20notes/ask/ask%201"
        )
      );
      expect(await view.findByText("Copied", { exact: true })).toBeTruthy();
    } finally {
      view.unmount();
    }
  });
});

test("CopyRefButton with a primary copies it on click and the reference on a modifier click", async () => {
  const user = userEvent.setup();
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  await withClipboard({ writeText }, async () => {
    const view = render(
      <CopyRefButton
        primary={{ label: "issue key LEGION-162", value: "LEGION-162" }}
        route={{ key: "LEGION-162", kind: "issue" }}
      />
    );
    try {
      const button = view.getByRole("button", { name: /^Copy issue key LEGION-162 · / });
      expect(button.getAttribute("title")).toMatch(
        /^Copy issue key LEGION-162 · (⌘|Ctrl)-click copies the reference$/
      );
      await user.click(button);
      await waitFor(() => expect(writeText).toHaveBeenNthCalledWith(1, "LEGION-162"));
      // A modifier click is the modifier held across the press: user-event's `click` takes no
      // modifier of its own.
      await user.keyboard("{Control>}");
      await user.click(button);
      await user.keyboard("{/Control}");
      await waitFor(() => expect(writeText).toHaveBeenNthCalledWith(2, "dispatch://LEGION-162"));
      await user.keyboard("{Meta>}");
      await user.click(button);
      await user.keyboard("{/Meta}");
      await waitFor(() => expect(writeText).toHaveBeenNthCalledWith(3, "dispatch://LEGION-162"));
      expect(writeText).toHaveBeenCalledTimes(3);
    } finally {
      view.unmount();
    }
  });
});

test("CopyRefButton prints the value it tried to copy when neither clipboard path works", async () => {
  const user = userEvent.setup();
  const originalExecCommand = document.execCommand;
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (_command: string) => false,
  });
  try {
    await withClipboard(undefined, async () => {
      const view = render(
        <CopyRefButton
          primary={{ label: "issue key LEGION-162", value: "LEGION-162" }}
          route={{ key: "LEGION-162", kind: "issue" }}
        />
      );
      try {
        const button = view.getByRole("button", { name: /^Copy issue key LEGION-162 · / });
        // A modifier click is the modifier held across the press: user-event's `click` takes no
        // modifier of its own.
        await user.keyboard("{Control>}");
        await user.click(button);
        await user.keyboard("{/Control}");
        const shown = await view.findByText("dispatch://LEGION-162", { selector: "code" });
        expect(shown.parentElement?.textContent).toBe("Copy failed - dispatch://LEGION-162");
        await user.click(button);
        const shownKey = await view.findByText("LEGION-162", { selector: "code" });
        expect(shownKey.parentElement?.textContent).toBe("Copy failed - LEGION-162");
      } finally {
        view.unmount();
      }
    });
  } finally {
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
  }
});
