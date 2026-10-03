// docs/site/media/broker/screenshots.spec.ts
//
// The broker's screenshots: the Dispatch pages a person uses to approve what an agent asks the
// broker for, taken against the rig (docs/site/media/broker/rig.sh) and its example agent
// machine, along the flow flow.ts names. Each shot is taken only once the state it shows is
// asserted, and the whole flow is checked end to end: the machine login's command exits 0 once
// approved, and the approved request's command runs with the secret.
import { mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { expect, test } from "@playwright/test";

import { signIn } from "../../../../packages/dispatch/e2e/users";
import { startMachineLogin, startSecretRequest, waitForOutput } from "./agent";
import { dispatch, machineLoginPath, operator, printed, rigState } from "./flow";

const assets = join(dirname(fileURLToPath(import.meta.url)), "../../public/media/broker");

test("the Dispatch pages a person approves broker requests on", async ({ browser }) => {
  test.setTimeout(300_000);
  const rig = rigState();
  mkdirSync(assets, { recursive: true });
  const context = await browser.newContext({
    baseURL: rig.dispatchUrl,
    deviceScaleFactor: 2,
    viewport: { height: 800, width: 1280 },
  });
  await signIn(context, operator);
  const page = await context.newPage();

  // The machine login: the code the machine printed, looked up and approved.
  const login = await startMachineLogin(rig.agentExec);
  await page.goto(machineLoginPath);
  await dispatch.codeField(page).fill(login.code);
  await dispatch.lookUp(page).click();
  await expect(dispatch.machineLoginRecord(page)).toBeVisible();
  await page.screenshot({ path: join(assets, "machine-login.png") });
  await dispatch.approve(page).click();
  await expect(dispatch.machineLoginApproved(page)).toBeVisible();
  expect((await login.done).code).toBe(0);

  // A secret request from a session on that machine: its Inbox row, its record, its approval.
  const request = await startSecretRequest(rig.agentExec);
  await page.goto("/");
  const row = dispatch.inboxRequest(page);
  await expect(row).toBeVisible();
  await page.screenshot({ path: join(assets, "inbox-credential-request.png") });
  await row.click();
  await expect(page).toHaveURL(new RegExp(`/credentials/${request.recordId}$`));
  await expect(dispatch.requestReason(page)).toBeVisible();
  await page.screenshot({ path: join(assets, "credential-request.png") });
  await dispatch.approve(page).click();
  await expect(dispatch.requestApproved(page)).toBeVisible();
  await waitForOutput(request, printed.keyReached, "the command's output");
  await page.screenshot({ path: join(assets, "credential-request-approved.png") });

  // The grant that approval made, naming its approver, under Settings' Live grants: live while
  // the session that asked is open.
  await page.goto("/settings");
  const grants = dispatch.liveGrants(page);
  await expect(grants.getByText("DEMO_API_KEY")).toBeVisible();
  await grants.screenshot({ path: join(assets, "live-grants.png") });

  request.end();
  const ran = await request.done;
  expect(ran.code, ran.stderr).toBe(0);
  await context.close();
});
