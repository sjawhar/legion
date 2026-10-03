import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { CredentialPendingRow } from "../../api/types";
import { BlockedOnYou } from "./BlockedOnYou";

function request(record_id: string, requested_at: string): CredentialPendingRow {
  return { identifiers: ["DEMO_API_KEY"], kind: "agent_secret", record_id, requested_at };
}

// The broker's `requested_at` reaches the SPA verbatim, and the count this rule gives also drives
// the shell's `Needs you N` badges, outside every page's error boundary.
test("a request whose time cannot be read still counts, and the banner's age comes from the rest", () => {
  const unreadable = request("record-1", "yesterday");
  const threeHoursAgo = request(
    "record-2",
    new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString()
  );

  const view = render(
    <MemoryRouter>
      <BlockedOnYou asks={[]} credentialRequests={[unreadable, threeHoursAgo]} />
    </MemoryRouter>
  );
  try {
    expect(screen.getByRole("link").textContent).toBe("Blocked on you: 2 items, oldest 3h");

    view.rerender(
      <MemoryRouter>
        <BlockedOnYou asks={[]} credentialRequests={[unreadable]} />
      </MemoryRouter>
    );
    expect(screen.getByRole("link").textContent).toBe("Blocked on you: 1 item");
  } finally {
    view.unmount();
  }
});
