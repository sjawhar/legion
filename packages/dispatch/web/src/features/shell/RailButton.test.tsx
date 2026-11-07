import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { RailButton } from "./RailButton";

test("RailButton shows its keys, announces them and opens what it stands for", async () => {
  const user = userEvent.setup();
  let opened = 0;
  const view = render(
    <RailButton
      ariaKeyshortcuts="C"
      className="mt-1"
      label="New issue"
      onOpen={() => (opened += 1)}
      shortcut="c"
    />
  );

  try {
    const button = screen.getByRole("button", { name: /new issue/i });
    expect(button).toHaveAttribute("aria-keyshortcuts", "C");
    expect(button.querySelector("kbd")?.textContent).toBe("c");
    await user.click(button);
    expect(opened).toBe(1);
  } finally {
    view.unmount();
  }
});
