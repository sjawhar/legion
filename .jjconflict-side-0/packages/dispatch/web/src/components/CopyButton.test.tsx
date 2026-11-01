import { expect, spyOn, test } from "bun:test";
import { fireEvent, render, waitFor } from "@testing-library/react";

import { CopyButton } from "./CopyButton";

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

test("CopyButton writes its value, confirms briefly, and returns to idle", async () => {
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  await withClipboard({ writeText }, async () => {
    const view = render(<CopyButton value="LEGION-2" what="issue key" />);
    try {
      const button = view.getByRole("button", { name: "Copy issue key LEGION-2" });
      expect(button.getAttribute("title")).toBe("Copy issue key LEGION-2");
      fireEvent.click(button);
      await waitFor(() => expect(writeText).toHaveBeenCalledWith("LEGION-2"));
      expect(await view.findByText("Copied", { exact: true })).toBeTruthy();
      await waitFor(() => expect(view.queryByText("Copied", { exact: true })).toBeNull(), {
        timeout: 10_000,
      });
    } finally {
      view.unmount();
    }
  });
}, 15_000);

test("CopyButton reports a failure when neither clipboard path can copy", async () => {
  const originalExecCommand = document.execCommand;
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (_command: string) => false,
  });
  try {
    await withClipboard(undefined, async () => {
      const view = render(
        <CopyButton value="dev:4.7" what="tmux target">
          dev:4.7
        </CopyButton>
      );
      try {
        fireEvent.click(view.getByRole("button", { name: "Copy tmux target dev:4.7" }));
        expect(
          await view.findByText("Copy failed - select the text", { exact: true })
        ).toBeTruthy();
        expect(view.getByText("dev:4.7", { exact: true })).toBeTruthy();
      } finally {
        view.unmount();
      }

      // A labelled or icon-only button does not show the value, so the hint prints it.
      const labelled = render(
        <CopyButton value="planner-session" what="session ID">
          ID
        </CopyButton>
      );
      try {
        fireEvent.click(labelled.getByRole("button", { name: "Copy session ID planner-session" }));
        const shownValue = await labelled.findByText("planner-session", { selector: "code" });
        expect(shownValue.parentElement?.textContent).toBe("Copy failed - planner-session");
        expect(labelled.queryByText("Copy failed - select the text", { exact: true })).toBeNull();
      } finally {
        labelled.unmount();
      }
    });
  } finally {
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
  }
});
