import { expect, type Locator, test } from "@playwright/test";

import type { RecordResponse } from "../web/src/api/types";
import { setBrokerDeviceGrant, setMachineLogins } from "./fake-broker-helpers";
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
const pendingLogin: RecordResponse = {
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
  // The broker names the session a request stands for; a machine login opened by
  // `agent-secrets machine login` has enrolled none and made none of its own.
  session: { enrollment: null, request: null },
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

// R7 of dispatch://INFRA-958: a broker started with no BROKER_MACHINE_LOGIN_URL serves no device
// grant, so no machine ever prints a code and Dispatch serves no page to type one into. Dispatch
// cannot reach the broker from a browser, so it relays that one fact out of the broker's RFC
// 8414 metadata (GET /api/v1/credentials/capabilities) and the app gates the route on it. Both
// answers are driven here against the same page, because "the page is gone" is only worth
// anything beside "the page is there".
//
// The waits are the server's cache of that document (`deviceGrantTTL`,
// packages/envoy/internal/dispatch/agentsecrets/client.go): a change at the broker reaches a
// reloaded page within it, which is what `expect.poll` waits for here rather than a fixed sleep.
// The row serves the grant again at the end, so it leaves the broker as it found it.
test("the machine-code page is served only while the broker serves the device grant", async ({
  browser,
}) => {
  test.setTimeout(60_000);
  await withAlice(browser, async (page) => {
    await page.goto("/credentials/machine");
    await expect(page.getByLabel("Code shown on the machine")).toBeVisible();

    // One reload, and what it settled on: the capability answer is out for a moment and the
    // page's own chunk for another, so each try waits for one of the two outcomes rather than
    // reading the instant the reload returns, which would find neither every time.
    const reloadShows = async (locator: Locator): Promise<boolean> => {
      await page.reload();
      return locator.waitFor({ state: "visible", timeout: 2_000 }).then(
        () => true,
        () => false
      );
    };

    await setBrokerDeviceGrant(false);
    await expect
      .poll(() => reloadShows(page.getByRole("heading", { name: "Page not found" })), {
        message: "the machine-code page is gone once the broker serves no device grant",
        timeout: 20_000,
      })
      .toBe(true);

    await setBrokerDeviceGrant(true);
    await expect
      .poll(() => reloadShows(page.getByLabel("Code shown on the machine")), {
        message: "the machine-code page is back once the broker serves the device grant again",
        timeout: 20_000,
      })
      .toBe(true);
  });
});
