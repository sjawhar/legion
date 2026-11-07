import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { DocumentToolbar } from "../doc/ProofDocument";
import { SpecToolbar } from "./SpecToolbar";

function renderToolbar(copied: boolean) {
  const toolbar: DocumentToolbar = {
    connection: "connected",
    copyBlockLink: async () => copied,
    isNamingVersion: false,
    pending: undefined,
    requestNamedVersion: () => {},
    versions: [],
  };
  return render(
    <SpecToolbar
      isClosed={false}
      onShowDiffChange={() => {}}
      onVersionChange={() => {}}
      reference={{ key: "CORE-1", kind: "spec" }}
      showDiff={false}
      toolbar={toolbar}
      version={undefined}
    />
  );
}

test("SpecToolbar reports whether copying a document link succeeded", async () => {
  const user = userEvent.setup();
  for (const [copied, message] of [
    [true, "Copied"],
    [false, "Copy failed - select the text"],
  ] as const) {
    const view = renderToolbar(copied);
    try {
      await user.click(screen.getByRole("button", { name: "Copy link to block" }));
      expect((await screen.findByText(message)).textContent).toBe(message);
    } finally {
      view.unmount();
    }
  }
});
