import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";

import { ApiError, api } from "../../api/client";
import { GrantsSection } from "./GrantsSection";
import { MachineLoginsSection } from "./MachineLoginsSection";

/** One section rendered through `RevocableList`, with its one listed row stubbed and its revoke
 *  call replaced by `revoke`; `clearList` makes the list answer no rows from then on, and `empty`
 *  is what the section shows then. */
interface Section {
  empty: string;
  heading: string;
  refusal: string;
  rowId: string;
  section: () => ReactNode;
  stub: (revoke: (id: string) => Promise<void>) => {
    clearList: () => void;
    revokeCalls: () => unknown[][];
    restore: () => void;
  };
}

const sections: Section[] = [
  {
    empty: "No live grants.",
    heading: "Live grants",
    refusal: "only the grant's approver or its enrollment's operator may revoke it",
    rowId: "grant-1",
    section: () => <GrantsSection />,
    stub: (revoke) => {
      const list = spyOn(api, "getCredentialGrants").mockResolvedValue({
        grants: [
          {
            approver: null,
            created_at: "2026-10-03T12:05:00Z",
            enrollment: {
              kind: "box",
              operator: "ada@example.com",
              runtime_id: "box-1",
              slot: null,
            },
            expires_at: "2026-10-03T13:05:00Z",
            grant_id: "grant-1",
            granted: "automatic",
            names: ["WORKER_TOKEN"],
            record_id: null,
          },
        ],
      });
      const spy = spyOn(api, "revokeCredentialGrant").mockImplementation(revoke);
      return {
        clearList: () => list.mockResolvedValue({ grants: [] }),
        restore: () => {
          list.mockRestore();
          spy.mockRestore();
        },
        revokeCalls: () => spy.mock.calls,
      };
    },
  },
  {
    empty: "No live machine logins.",
    heading: "Your machine logins",
    refusal: "only the person who approved the machine login may revoke it",
    rowId: "cred-devbox",
    section: () => <MachineLoginsSection />,
    stub: (revoke) => {
      const list = spyOn(api, "getMachineLogins").mockResolvedValue({
        credentials: [
          {
            credential_id: "cred-devbox",
            expired: false,
            expires_at: "2026-10-10T12:00:00Z",
            host: "devbox.example.com",
            issued_at: "2026-10-03T12:00:00Z",
            service: null,
          },
        ],
      });
      const spy = spyOn(api, "revokeMachineLogin").mockImplementation(revoke);
      return {
        clearList: () => list.mockResolvedValue({ credentials: [] }),
        restore: () => {
          list.mockRestore();
          spy.mockRestore();
        },
        revokeCalls: () => spy.mock.calls,
      };
    },
  },
];

async function renderSection(section: Section): Promise<HTMLElement> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={queryClient}>{section.section()}</QueryClientProvider>);
  const region = within(await screen.findByRole("region", { name: section.heading }));
  return region.findByRole("button", { name: "Revoke" });
}

/** Lets a mutation reach its function: it calls it a few microtasks after the click, and a timer
 *  runs after them all. */
async function afterMicrotasks(): Promise<void> {
  const settled = Promise.withResolvers<void>();
  setTimeout(settled.resolve, 0);
  await settled.promise;
}

for (const section of sections) {
  test(`${section.heading}: Revoke is disabled while a revoke is out, and enabled once it settles`, async () => {
    const out = Promise.withResolvers<void>();
    const stubs = section.stub(() => out.promise);
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    try {
      const button = await renderSection(section);
      fireEvent.click(button);
      await waitFor(() => expect(stubs.revokeCalls()).toEqual([[section.rowId]]));
      await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(true));

      out.resolve();
      const region = within(screen.getByRole("region", { name: section.heading }));
      await waitFor(() =>
        expect((region.getByRole("button", { name: "Revoke" }) as HTMLButtonElement).disabled).toBe(
          false
        )
      );
    } finally {
      cleanup();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  test(`${section.heading}: a double press before the button disables sends one revoke`, async () => {
    const out = Promise.withResolvers<void>();
    const stubs = section.stub(() => out.promise);
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    try {
      const button = await renderSection(section);
      // Both presses land inside one act, so React re-renders only after both: the second reaches
      // the handler before the pending revoke disables the button, which only the guard stops.
      act(() => {
        fireEvent.click(button);
        fireEvent.click(button);
      });
      await afterMicrotasks();
      expect(stubs.revokeCalls()).toEqual([[section.rowId]]);
      out.resolve();
    } finally {
      cleanup();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  test(`${section.heading}: a refused revoke shows the broker's refusal`, async () => {
    const stubs = section.stub(async () => {
      throw new ApiError(403, { code: "NOT_APPROVER", error: section.refusal });
    });
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    try {
      fireEvent.click(await renderSection(section));
      expect(await screen.findByText(section.refusal)).toBeDefined();
    } finally {
      cleanup();
      confirm.mockRestore();
      stubs.restore();
    }
  });

  test(`${section.heading}: a revoke refreshes the list, so the revoked row leaves it`, async () => {
    // The revoke answers as the broker would: once it succeeds, the list no longer holds the row.
    const stubs = section.stub(async () => stubs.clearList());
    const confirm = spyOn(window, "confirm").mockReturnValue(true);
    try {
      fireEvent.click(await renderSection(section));
      const region = within(screen.getByRole("region", { name: section.heading }));
      expect(await region.findByText(section.empty)).toBeDefined();
      expect(region.queryByRole("button", { name: "Revoke" })).toBeNull();
      expect(stubs.revokeCalls()).toEqual([[section.rowId]]);
    } finally {
      cleanup();
      confirm.mockRestore();
      stubs.restore();
    }
  });
}
