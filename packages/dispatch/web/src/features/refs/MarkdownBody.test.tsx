import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { MarkdownBody } from "./MarkdownBody";

test("a code span containing tag-shaped text renders the literal characters with no backslashes", async () => {
  const view = render(<MarkdownBody markdown="Use `<img src=x>` here" />);

  try {
    const code = await screen.findByText("<img src=x>", { selector: "code" });
    expect(code.textContent).toBe("<img src=x>");
  } finally {
    view.unmount();
  }
});

test("raw unsupported HTML renders as literal text with no element created", async () => {
  const view = render(<MarkdownBody markdown="<img src=x onerror=alert(1)>" />);

  try {
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).not.toBeNull();
    expect(view.container.querySelector("img")).toBeNull();
  } finally {
    view.unmount();
  }
});
