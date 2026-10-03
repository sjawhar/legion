// docs/site/media/broker/flow.ts
//
// The flow the broker's screenshots and walkthrough both show, written once: what the rig hands
// its command, the demo's names, the request's reason, what the agent machine prints that a
// person acts on, and the Dispatch pages and controls a person uses to approve it.
import type { Locator, Page } from "@playwright/test";

/** The e2e workspace's human: the agent machine's operator and every request's approver. */
export const operator = "alice";
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

/** What the agent machine prints: the machine login's code, a pending request's id and the
 *  Dispatch record a person decides it on, and the demo command's line once the key reached it. */
export const printed = {
  loginCode: /machine login code: ([A-Z0-9]{4}-[A-Z0-9]{4})/,
  requestWaiting: /request (\S+) is waiting for approval/,
  recordLink: /\/credentials\/([0-9a-f]{64})/,
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
  inboxRequest: (page: Page): Locator =>
    page.getByRole("link", { name: /Secret request.*DEMO_API_KEY/s }),
  requestReason: (page: Page): Locator => page.getByText(reason),
  requestApproved: (page: Page): Locator => page.getByText(/^approved/i),
  settings: (page: Page): Locator => page.getByRole("link", { name: "Settings" }),
  liveGrants: (page: Page): Locator =>
    page.locator("section[aria-labelledby='credential-grants-heading']"),
};
