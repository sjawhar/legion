import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";

import type { DocumentToolbar } from "../doc/ProofDocument";
import { SpecToolbar } from "./SpecToolbar";

function renderToolbar(copied: boolean) {
  const toolbar: DocumentToolbar = {
    connection: "connected",
    copyBlockLink: async () => copied,
    isNamingVersion: false,
    requestNamedVersion: () => {},
    versions: [],
  };
  return render(
    <SpecToolbar
      isClosed={false}
      onShowDiffChange={() => {}}
      onVersionChange={() => {}}
      showDiff={false}
      toolbar={toolbar}
      version={undefined}
    />
  );
}

test("SpecToolbar reports whether copying a document link succeeded", async () => {
  for (const [copied, message] of [
    [true, "Copied"],
    [false, "Copy failed - select the text"],
  ] as const) {
    const view = renderToolbar(copied);
    try {
      fireEvent.click(screen.getByRole("button", { name: "Copy link to block" }));
      expect((await screen.findByText(message)).textContent).toBe(message);
    } finally {
      view.unmount();
    }
  }
});
