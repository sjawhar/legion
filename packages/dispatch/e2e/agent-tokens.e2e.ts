import { expect, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

const e2ePort = process.env.DISPATCH_E2E_PORT || "8777";
const baseUrl = process.env.PLAYWRIGHT_BASE_URL || `http://127.0.0.1:${e2ePort}`;

async function createAskWithToken(issue: string, token: string): Promise<Response> {
  return fetch(new URL(`/api/v1/issues/${issue}/asks`, baseUrl), {
    body: JSON.stringify({
      actor: { id: "architect-session", kind: "session", origin: { session_title: "Architect" } },
      question: "Which release gate should we use?",
    }),
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    method: "POST",
  });
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a personal agent token attributes its ask, remains private, and is revoked from Settings", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Agent token attribution" });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  const alicePage = await alice.newPage();
  const bobPage = await bob.newPage();

  try {
    await alicePage.goto("/settings");
    await alicePage.getByLabel("Token label").fill("Architect");
    await alicePage.getByRole("button", { name: "New token" }).click();
    const token = await alicePage.getByRole("status").locator("code").first().textContent();
    if (token === null) {
      throw new Error("new agent token was not rendered");
    }
    expect(token).toMatch(/^dsp_[A-Za-z0-9_-]{43}$/);
    const tokenPrefix = `dsp_${token.slice(4, 12)}…`;

    const created = await createAskWithToken(issue.key, token);
    expect(created.status).toBe(201);

    await alicePage.goto("/");
    await expect(alicePage.getByText("Architect (for alice)")).toBeVisible();

    await bobPage.goto("/settings");
    await expect(bobPage.getByRole("cell", { name: tokenPrefix })).toHaveCount(0);
    await expect(bobPage.getByText("No agent tokens yet.")).toBeVisible();

    alicePage.once("dialog", (dialog) => dialog.accept());
    await alicePage.goto("/settings");
    await alicePage.getByRole("button", { name: "Revoke Architect" }).click();
    await expect(
      alicePage
        .getByRole("row", { name: new RegExp(`Architect ${tokenPrefix}`) })
        .getByText("Revoked")
    ).toBeVisible();

    const revoked = await createAskWithToken(issue.key, token);
    expect(revoked.status).toBe(401);
    expect(await revoked.json()).toMatchObject({ code: "TOKEN_REVOKED" });
  } finally {
    await alice.close();
    await bob.close();
  }
});
