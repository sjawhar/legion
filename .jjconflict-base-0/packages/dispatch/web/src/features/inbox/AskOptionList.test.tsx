import { expect, test } from "bun:test";
import { render } from "@testing-library/react";

import type { AskOption } from "../../api/types";
import { AskOptionList } from "./AskOptionList";

test("an option label with backticks renders inline code with no wrapping <p> or literal backticks", async () => {
  const options: AskOption[] = [{ label: "Ship it `now`" }];
  const view = render(<AskOptionList options={options} selected={[]} />);

  try {
    const code = await view.findByText("now", { selector: "code" });
    expect(code).toBeTruthy();
    expect(code.closest("p")).toBeNull();
    expect(view.getByRole("listitem").textContent).toBe("Ship it now");
  } finally {
    view.unmount();
  }
});

test("a list-only option label flattens to inline text with no nested list item", async () => {
  const options: AskOption[] = [{ label: "- a" }];
  const view = render(<AskOptionList options={options} selected={[]} />);

  try {
    await view.findByText("a");
    const items = view.getAllByRole("listitem");
    expect(items).toHaveLength(1);
    expect(items[0]?.textContent).toBe("a");
  } finally {
    view.unmount();
  }
});
