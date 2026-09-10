import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { SelectionMenu } from "./SelectionMenu";

test("selection actions stay in the viewport for an off-screen document selection", () => {
  const height = window.innerHeight;
  Object.defineProperty(window, "innerHeight", { configurable: true, value: 664 });
  const view = render(
    <SelectionMenu
      onAction={() => {}}
      selection={{
        artifact: "artifact-1",
        artifactId: "artifact-1",
        canSuggest: true,
        quote: "brown",
        rect: { bottom: 1017, left: 100, right: 150, top: 997 },
      }}
    />
  );

  try {
    expect(screen.getByRole("toolbar").style.top).toBe("620px");
  } finally {
    view.unmount();
    Object.defineProperty(window, "innerHeight", { configurable: true, value: height });
  }
});
