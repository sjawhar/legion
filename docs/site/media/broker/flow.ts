// docs/site/media/broker/flow.ts
//
// The flow the broker's screenshots and walkthrough both show, written once: what the rig hands
// its command, the demo's names, the request's reason, what the agent machine prints that a
// person acts on, the Dispatch pages and controls a person uses to log the machine in and find the
// request, and the broker's own page where they approve it.
import type { Locator, Page } from "@playwright/test";

/** The harness's signed-in human: the agent machine's operator, and the owner and so the approver
 *  of the demo's one secret. */
export const operator = "alice@example.com";
export const agentHost = "example-host-build";
export const reason = "Publish the docs preview for PR 42 with the demo API";

export interface RigState {
  /** rig.sh's agent-exec: runs its arguments on the agent machine, in its demo directory. */
  agentExec: string;
  dispatchUrl: string;
}

/** What `rig.sh -- <command>` hands its command: BROKER_RIG_AGENT_EXEC and BROKER_RIG_DISPATCH_URL. */
export function rigState(): RigState {
  const value = (name: string): string => {
    const found = process.env[name];
    if (found === undefined || found === "") {
      throw new Error(
        `${name} is unset: run this under docs/site/media/broker/rig.sh -- <command>`
      );
    }
    return found;
  };
  return {
    agentExec: value("BROKER_RIG_AGENT_EXEC"),
    dispatchUrl: value("BROKER_RIG_DISPATCH_URL"),
  };
}

/** What the agent machine prints: the machine login's code, a pending request's id and its page on
 *  the broker (the whole address, then the record id it ends in), and the demo command's line once
 *  the key reached it. */
export const printed = {
  loginCode: /machine login code: ([A-Z0-9]{4}-[A-Z0-9]{4})/,
  requestWaiting: /request (\S+) is waiting for approval/,
  approvePage: /(https?:\/\/\S+\/approve\/([0-9a-f]{64}))/,
  keyReached: /DEMO_API_KEY reached this command/,
};

/** Where the operator types the machine login's code. */
export const machineLoginPath = "/credentials/machine";

/** The Dispatch controls and results the flow passes through, in its order. */
export const dispatch = {
  machineLoginHeading: (page: Page): Locator =>
    page.getByRole("heading", { name: "Enter machine login code" }),
  codeField: (page: Page): Locator => page.getByLabel("Code shown on the machine"),
  lookUp: (page: Page): Locator => page.getByRole("button", { name: "Look up" }),
  machineLoginRecord: (page: Page): Locator =>
    page.getByText(`Approving lets ${agentHost} start agent sessions as you.`),
  approve: (page: Page): Locator => page.getByRole("button", { name: "Approve" }),
  machineLoginApproved: (page: Page): Locator =>
    page.getByText(`Approved. ${agentHost} can start agent sessions as you.`),
  /** The Inbox's row for the request: a link to its page on the broker. */
  inboxRequest: (page: Page): Locator =>
    page.getByRole("link", { name: /Secret request.*DEMO_API_KEY/s }),
  settings: (page: Page): Locator => page.getByRole("link", { name: "Settings" }),
  liveGrants: (page: Page): Locator =>
    page.locator("section[aria-labelledby='credential-grants-heading']"),
};

/** The broker's approval page (`<broker>/approve/<record>`) and what it shows. */
export const broker = {
  /** The header line naming who is signed in to the broker. */
  signedIn: (page: Page): Locator =>
    page.getByRole("banner").getByText(`Signed in as ${operator}.`),
  requestReason: (page: Page): Locator => page.getByText(reason),
  /** The record's own Approve; "Approve all N shown" is another button. */
  approve: (page: Page): Locator => page.getByRole("button", { name: "Approve", exact: true }),
  /** The line the page shows, under What happened, once the approval made the grant. */
  requestGranted: (page: Page): Locator =>
    page.locator(".outcomes").getByText(`DEMO_API_KEY: granted (approved by ${operator} at`),
};
