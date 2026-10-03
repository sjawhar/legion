// docs/site/media/broker/seed.ts
//
// Seeds the Dispatch e2e workspace the broker's screenshots and walkthrough show around the
// credential requests: the same reset and the same workspace the e2e suite builds
// (packages/dispatch/e2e/seed.ts, workspace.ts), through the harness's public API.
import { resetDatabase } from "../../../../packages/dispatch/e2e/seed";
import { seedWorkspace } from "../../../../packages/dispatch/e2e/workspace";

await resetDatabase();
await seedWorkspace();
