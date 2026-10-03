// docs/site/media/broker/screenshots.spec.ts
//
// The broker's screenshots: the Dispatch pages a person uses to approve what an agent asks the
// broker for, taken against the rig (docs/site/media/broker/rig.sh), its seeded e2e workspace and
// its example agent machine. Each shot is taken only once the state it shows is asserted, and the
// whole flow is checked end to end: the machine login's command exits 0 once approved, and the
// approved request's command runs with the secret.
import { mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { expect, test } from "@playwright/test";

import { rigState, startMachineLogin, startSecretRequest, waitForOutput } from "./agent";

const assets = join(dirname(fileURLToPath(import.meta.url)), "../../public/media/broker");
const reason = "Publish the docs preview for PR 42 with the demo API.";

test("the Dispatch pages a person approves broker requests on", async ({ browser }) => {
  test.setTimeout(300_000);
  const rig = rigState();
  mkdirSync(assets, { recursive: true });
  const context = await browser.newContext({
    baseURL: rig.dispatchUrl,
    deviceScaleFactor: 2,
    extraHTTPHeaders: { "X-Dispatch-User": rig.operator },
    viewport: { height: 800, width: 1280 },
  });
  const page = await context.newPage();

  // The machine login: the code the machine printed, looked up and approved.
  const login = await startMachineLogin(rig.agentExec);
  await page.goto("/credentials/machine");
  await page.getByLabel("Code shown on the machine").fill(login.code);
  await page.getByRole("button", { name: "Look up" }).click();
  await expect(
    page.getByText("Approving lets example-host-build start agent sessions as you.")
  ).toBeVisible();
  await page.screenshot({ path: join(assets, "machine-login.png") });
  await page.getByRole("button", { name: "Approve" }).click();
  await expect(
    page.getByText("Approved. example-host-build can start agent sessions as you.")
  ).toBeVisible();
  expect((await login.done).code).toBe(0);

  // A secret request from a session on that machine: its Inbox row, its record, its approval.
  const request = await startSecretRequest(rig.agentExec, reason);
  await page.goto("/");
  const row = page.getByRole("link", { name: /Secret request.*DEMO_API_KEY/s });
  await expect(row).toBeVisible();
  await page.screenshot({ path: join(assets, "inbox-credential-request.png") });
  await row.click();
  await expect(page).toHaveURL(new RegExp(`/credentials/${request.recordId}$`));
  await expect(page.getByText(reason)).toBeVisible();
  await page.screenshot({ path: join(assets, "credential-request.png") });
  await page.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText(/^approved/i)).toBeVisible();
  await waitForOutput(request, /DEMO_API_KEY reached this command/, "the command's output");
  await page.screenshot({ path: join(assets, "credential-request-approved.png") });

  // The grant that approval made, naming its approver, under Settings' Live grants: live while
  // the session that asked is open.
  await page.goto("/settings");
  const grants = page.locator("section[aria-labelledby='credential-grants-heading']");
  await expect(grants.getByText("DEMO_API_KEY")).toBeVisible();
  await grants.screenshot({ path: join(assets, "live-grants.png") });

  request.end();
  const ran = await request.done;
  expect(ran.code, ran.stderr).toBe(0);
  await context.close();
});
