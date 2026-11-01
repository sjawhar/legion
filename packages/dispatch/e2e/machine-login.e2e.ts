import { expect, test } from "@playwright/test";

import type { CredentialRecord } from "../web/src/api/types";
import { setMachineLogins } from "./fake-broker-helpers";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** The code as the operator types it off the machine's screen: the page upper-cases it and puts
 *  the hyphen back, and the broker normalizes it again before it looks it up. */
const typedCode = "wxyz4321";

/** The machine login `agent-secrets machine login` opened, as the broker's lookup answers it:
 *  waiting on alice, who is the only person it admits. */
const pendingLogin: CredentialRecord = {
  approver: "alice",
  decided: null,
  enrollment: null,
  expires_at: "2026-09-27T01:00:00Z",
  identifiers: ["worker-7.example.com"],
  kind: "launcher_credential",
  lifetime_seconds: 900,
  reason: "",
  record_id: "record-machine-login",
  requested_at: "2026-09-27T00:00:00Z",
  rules_version: "v3",
  service: null,
  state: "pending",
};

// A7's last clause: "Dispatch's machine-login page still finds and decides it in Dispatch's
// browser suite." This rollout step moves a secret request's approval to the secrets broker's own
// page and leaves a machine login where it has always been, so a browser must drive the whole of
// that path: the operator types the code the machine printed, Dispatch relays the lookup and then
// the approval to the broker (here e2e/fake-broker.ts, which the harness server reads), and the
// broker keeps the decision. The fake broker refuses a decision from anyone the record does not
// admit (NOT_APPROVER), so an approval that goes through is one Dispatch named alice on. The
// reload at the end drops everything the page knew: the decision it shows afterwards can only
// have come from the broker's own record.
test("the machine-login page finds a pending login by its typed code and Approve decides it on the broker", async ({
  browser,
}) => {
  await withAlice(browser, async (page) => {
    await setMachineLogins([{ code: "WXYZ-4321", record: pendingLogin }]);

    await page.goto("/credentials/machine");
    const code = page.getByLabel("Code shown on the machine");
    await code.fill(typedCode);
    await expect(code).toHaveValue("WXYZ-4321");
    await page.getByRole("button", { name: "Look up" }).click();

    await expect(page.getByText("worker-7.example.com", { exact: true })).toBeVisible();
    await expect(page.getByText("Machine login", { exact: true })).toBeVisible();
    await expect(page.getByText("15 minutes", { exact: true })).toBeVisible();
    await expect(
      page.getByText("Approving lets worker-7.example.com start agent sessions as you.")
    ).toBeVisible();

    await page.getByRole("button", { name: "Approve" }).click();
    await expect(
      page.getByText("Approved. worker-7.example.com can start agent sessions as you.")
    ).toBeVisible();
    await expect(page.getByText("Credential cred-record-machine-login")).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Deny" })).toHaveCount(0);

    await page.reload();
    await page.getByLabel("Code shown on the machine").fill(typedCode);
    await page.getByRole("button", { name: "Look up" }).click();
    await expect(
      page.getByText("Approved. worker-7.example.com can start agent sessions as you.")
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve" })).toHaveCount(0);
  });
});
