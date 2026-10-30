import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";

import { CreateIssueButton } from "./CreateIssueButton";

test("CreateIssueButton announces its shortcut and opens the dialog", () => {
  let opened = 0;
  const view = render(<CreateIssueButton onOpen={() => (opened += 1)} />);

  try {
    const button = screen.getByRole("button", { name: /new issue/i });
    expect(button.getAttribute("aria-keyshortcuts")).toBe("C");
    fireEvent.click(button);
    expect(opened).toBe(1);
  } finally {
    view.unmount();
  }
});
