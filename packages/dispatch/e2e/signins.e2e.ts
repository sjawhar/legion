import { expect, test } from "@playwright/test";

import type { FakeSignIn } from "./fake-broker";
import { setSignIns } from "./fake-broker-helpers";
import { resetDatabase } from "./seed";
import { withAlice } from "./users";

test.beforeEach(async () => {
  await resetDatabase();
});

/** Alice's two sign-ins, one per machine she ran `agent-secrets login` on, as the broker lists
 *  them. Bob's is seeded beside them on a machine account of its own, so the page proves it
 *  shows one person's sign-ins and not the broker's whole table. */
const aliceLaptop: FakeSignIn = {
  created_at: "2026-10-28T09:00:00Z",
  last_used_at: "2026-11-01T18:30:00Z",
  machine: "workstations@example-project.iam.gserviceaccount.com",
  person: "alice",
  signin_id: "11111111-1111-4111-8111-111111111111",
  thumbprint: "aaaabbbbccccddddeeeeffffgggghhhhiiiijjjjkkk",
};
const aliceBuildBox: FakeSignIn = {
  created_at: "2026-10-30T11:15:00Z",
  last_used_at: "2026-11-02T07:45:00Z",
  machine: "builders@example-project.iam.gserviceaccount.com",
  person: "alice",
  signin_id: "22222222-2222-4222-8222-222222222222",
  thumbprint: "lllmmmmnnnnooooppppqqqqrrrrssssttttuuuuvvv",
};
const bobLaptop: FakeSignIn = {
  created_at: "2026-10-29T10:00:00Z",
  last_used_at: "2026-11-01T10:00:00Z",
  machine: "reviewers@example-project.iam.gserviceaccount.com",
  person: "bob",
  signin_id: "33333333-3333-4333-8333-333333333333",
  thumbprint: "wwwwxxxxyyyyzzzz0000111122223333444455556",
};

// R4 of dispatch://INFRA-958: "Dispatch lists each person's sign-ins, by machine, on a page a
// person reads, and the person revokes one there." A sign-in is a person's standing permission
// for one machine to act as them, so the page that ends one has to work in a browser: alice
// reads her own two machines, revokes one, and that row is gone — from the page and from the
// broker, since the reload at the end drops everything the page knew and the list it shows
// afterwards can only have come from the broker's own record. Bob's sign-in is in the broker the
// whole time and never on her page: Dispatch names the person from its own session, so nobody
// reads anyone else's.
test("the sign-ins section lists a person's own machines and Revoke ends one on the broker", async ({
  browser,
}) => {
  await withAlice(browser, async (page) => {
    await setSignIns([aliceLaptop, aliceBuildBox, bobLaptop]);

    await page.goto("/settings");
    const signIns = page.getByRole("region", { name: "Sign-ins" });
    // The header row and one row per sign-in of hers.
    await expect(signIns.getByRole("row")).toHaveCount(3);
    const laptopRow = signIns.getByRole("row").filter({ hasText: aliceLaptop.machine });
    const buildBoxRow = signIns.getByRole("row").filter({ hasText: aliceBuildBox.machine });
    await expect(laptopRow).toHaveCount(1);
    await expect(buildBoxRow).toHaveCount(1);
    await expect(signIns.getByRole("row").filter({ hasText: bobLaptop.machine })).toHaveCount(0);
    // The box key is what tells two of one person's machines apart, so the row shows it.
    await expect(laptopRow.getByText(`key ${aliceLaptop.thumbprint.slice(0, 12)}…`)).toBeVisible();

    page.once("dialog", (dialog) => void dialog.accept());
    await buildBoxRow.getByRole("button", { name: "Revoke" }).click();
    await expect(buildBoxRow).toHaveCount(0);
    await expect(laptopRow).toHaveCount(1);

    await page.reload();
    const reloaded = page.getByRole("region", { name: "Sign-ins" });
    await expect(reloaded.getByRole("row")).toHaveCount(2);
    await expect(reloaded.getByRole("row").filter({ hasText: aliceLaptop.machine })).toHaveCount(1);
    await expect(reloaded.getByRole("row").filter({ hasText: aliceBuildBox.machine })).toHaveCount(
      0
    );
  });
});
