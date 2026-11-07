import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { CredentialGrant } from "../../api/types";
import { GrantsSection } from "./GrantsSection";

/** A grant on a session the viewer operates: the broker allows the operator's revoke, so the row
 *  offers one. */
const operated: CredentialGrant = {
  approver: null,
  created_at: "2026-10-03T12:05:00Z",
  enrollment: { kind: "session", operator: "ada@example.com", runtime_id: "box-1", slot: null },
  expires_at: "2026-10-03T13:05:00Z",
  grant_id: "grant-operated",
  granted: "automatic",
  names: ["WORKER_TOKEN"],
  record_id: null,
};

/** A grant that belongs to no person: the policy gave it without asking, so it has no approver,
 *  and a pod acts for nobody, so its enrollment has no operator. The broker refuses every
 *  signed-in person's revoke of it. */
function nobodys(operator: string | null): CredentialGrant {
  return {
    approver: null,
    created_at: "2026-10-03T12:06:00Z",
    enrollment: { kind: "pod", operator, runtime_id: "pod-7", slot: null },
    expires_at: "2026-10-03T13:06:00Z",
    grant_id: "grant-nobodys",
    granted: "automatic",
    names: ["SHARED_TOKEN"],
    record_id: null,
  };
}

// A grant with neither an approver nor an operator is on every signed-in person's page so that
// what a pod took is visible, and RevokeByApprover's mayRevoke refuses all of them for it. A
// Revoke button there could only fail, so the row is read-only and says what does end it.
//
// Both ways the wire says "no operator" are driven. The broker's recordEnrollmentResp.Operator is
// a Go string, so a real pod grant arrives as "" and never as null, and a row read only for null
// would leave every real one with the button it cannot use.
for (const operator of ["", null]) {
  test(`a grant whose operator is ${JSON.stringify(operator)} and approver is null is read-only, and the viewer's own still revokes`, async () => {
    const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
      grants: [operated, nobodys(operator)],
    });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    try {
      render(
        <QueryClientProvider client={queryClient}>
          <GrantsSection />
        </QueryClientProvider>
      );
      const section = within(await screen.findByRole("region", { name: "Live grants" }));

      const nobodysRow = (await section.findByText("SHARED_TOKEN")).closest("tr") as HTMLElement;
      expect(within(nobodysRow).queryByRole("button", { name: "Revoke" })).toBeNull();
      expect(within(nobodysRow).getByText("Ends with its machine login")).toBeDefined();

      const operatedRow = section.getByText("WORKER_TOKEN").closest("tr") as HTMLElement;
      expect(within(operatedRow).getByRole("button", { name: "Revoke" })).toBeDefined();
      expect(within(operatedRow).queryByText("Ends with its machine login")).toBeNull();

      expect(section.getAllByRole("button", { name: "Revoke" })).toHaveLength(1);
    } finally {
      cleanup();
      getCredentialGrants.mockRestore();
    }
  });
}
