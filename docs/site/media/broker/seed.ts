// docs/site/media/broker/seed.ts
//
// Empties the Dispatch e2e database for the broker's screenshots and walkthrough, through the
// harness's own reset (packages/dispatch/e2e/seed.ts). The flow they show needs no workspace
// content: alice@example.com signs in by the harness's dev route, and the machine login, the
// request and the grant all come from the broker. So the Inbox and Settings they show hold nothing
// else.
import { resetDatabase } from "../../../../packages/dispatch/e2e/seed";

await resetDatabase();
