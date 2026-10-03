// Example issues going through Legion, as Dispatch records them. Each move below is one the Legion
// daemon, its agents or the human makes, written through the API they write through, so the Legion
// section's screenshots (`shots.config.ts` beside this file) and its narrated walkthrough
// (`../walkthroughs/legion-issue-journey.ts`) show what a real journey leaves in Dispatch. Every
// name, key, commit and URL is example data.
import { dispatchApi, expect, fakeEnvoy } from "../harness";

export const PROJECT = "SHOP";

/** The Legion daemon's own writes: the status it sets and the READY it posts. */
export const DAEMON = {
  actor: { id: "legion-daemon:SHOP", kind: "session" },
  as: "agent",
} as const;

/** The project's controller session, which posts the daily report. */
export const CONTROLLER = {
  actor: { id: "shop-controller", kind: "session", origin: { session_title: "SHOP controller" } },
  as: "agent",
} as const;

export type AgentRole = "architect" | "implementer";

/** One of an issue's Legion sessions, named as Legion names its panes: `<KEY> <role>`. */
export function agent(key: string, role: AgentRole) {
  const session = `${key.toLowerCase()}-${role}`;
  return {
    actor: { id: session, kind: "session", origin: { session_title: `${key} ${role}` } },
    as: "agent",
  } as const;
}

/** What one example issue says at each step of its journey. */
export interface Example {
  readonly title: string;
  readonly spec: string;
  /** The design the architect adds to the spec, with one decision block. */
  readonly design: string;
  /** The decision block's id and the option the human picks. */
  readonly decision: { readonly id: string; readonly pick: string };
  readonly approvalSummary: string;
  readonly pullRequest: { readonly number: number; readonly url: string };
  /** The approved head the merger names in READY. */
  readonly head: string;
  readonly ready: { readonly outcome: string; readonly risk: string };
  readonly merge: string;
  readonly production: string;
  readonly signOff: string;
}

export const SAVED_CART: Example = {
  title: "Let shoppers save their cart for later",
  spec: `# Save a cart for later

A signed-in shopper can save the cart they are building and pick it up again later, on any device.

## Acceptance criteria

- A shopper with items in their cart can save it from the cart page.
- A saved cart opens from their account on another device, with the same items and quantities.
- A saved cart untouched for 30 days is removed, and the shopper is emailed a week before.
`,
  design: `## Design

Saving copies the cart's lines into a saved cart the account owns. Opening a saved cart replaces the
current cart, after asking first when the current cart has items.

:::ask{#cart-storage urgency="high" multiple="false"}
Where should a saved cart live?

I recommend Server: the second criterion needs the cart on another device, which browser storage cannot give.

- Server: one saved cart per account in the database, so any device can open it
- Browser: local storage on the device that saved it, with no schema change
:::
`,
  decision: { id: "cart-storage", pick: "Server" },
  approvalSummary:
    "Saved carts live on the server, one per account, and open on any device; a nightly job removes carts untouched for 30 days, a week after emailing the shopper.",
  pullRequest: { number: 42, url: "https://github.com/acme/storefront/pull/42" },
  head: "9c41e07",
  ready: {
    outcome:
      "a signed-in shopper saves their cart and reopens it from their account on another device.",
    risk: "the expiry email was checked in the mail provider's sandbox, not a real inbox.",
  },
  merge: "4b7d2a1",
  production:
    "Production: on the live storefront, saved a two-item cart as a test shopper, opened it from the account page in a second browser, and saw both items at their quantities. Merge commit 4b7d2a1.",
  signOff:
    "Signed off. All three acceptance criteria are met; review approved 9c41e07 with no open threads; retro is done; merged as 4b7d2a1; the production record shows the saved cart opening on a second device.",
};

export const DELIVERY_DATE: Example = {
  title: "Show the delivery date at checkout",
  spec: `# Show the delivery date at checkout

A shopper sees when their order will arrive before they pay.

## Acceptance criteria

- Each shipping option at checkout shows the date the order would arrive.
- Choosing another shipping option shows that option's date.
`,
  design: `## Design

The checkout page asks for a date for every shipping option when it loads, and shows each beside its
option.

:::ask{#date-source urgency="high" multiple="false"}
Where should the delivery dates come from?

I recommend Carrier: its estimate already counts the warehouse's cut-off time and the carrier's holidays.

- Carrier: the carrier's estimate for each shipping option
- Table: business days per shipping option, kept in the store's settings
:::
`,
  decision: { id: "date-source", pick: "Carrier" },
  approvalSummary:
    "Checkout asks the carrier for each shipping option's delivery date when it loads, and shows the date beside the option.",
  pullRequest: { number: 43, url: "https://github.com/acme/storefront/pull/43" },
  head: "2f8e6c1",
  ready: {
    outcome: "each shipping option at checkout shows the carrier's delivery date.",
    risk: "none",
  },
  merge: "7d03b9e",
  production:
    "Production: on the live storefront, opened checkout with one item as a test shopper and saw a delivery date beside each of the three shipping options. Merge commit 7d03b9e.",
  signOff:
    "Signed off. Both acceptance criteria are met; review approved 2f8e6c1 with no open threads; retro is done; merged as 7d03b9e; the production record shows the dates at checkout.",
};

/** An example issue in Dispatch: its key and its spec document. */
export interface Tracked {
  readonly key: string;
  readonly spec: string;
}

/** Creates the project and marks every Legion session the examples name live in the fake Envoy,
 *  so Dispatch shows each as a running agent. */
export async function seedProject(keys: readonly string[]): Promise<void> {
  const api = await dispatchApi();
  const { setLiveSessions } = await fakeEnvoy();
  await setLiveSessions(
    keys.flatMap((key) =>
      (["architect", "implementer"] as const).map((role) => ({
        capabilities: ["aside", "btw", "steer"],
        dir: `/workspaces/${key.toLowerCase()}-${role}`,
        machine_id: "example-host-legion",
        roles: [`legion-shop-${key.toLowerCase()}-${role}`],
        session_id: `${key.toLowerCase()}-${role}`,
        title: `${key} ${role}`,
      }))
    )
  );
  await api.createProject({ key: PROJECT, name: "Storefront" });
}

/** A human files the issue with its spec. */
export async function file(example: Example): Promise<Tracked> {
  const api = await dispatchApi();
  const issue = await api.createIssue({
    project: PROJECT,
    spec: example.spec,
    title: example.title,
  });
  return { key: issue.key, spec: issue.primary_artifact_id };
}

/** The handover: a human labels the issue `legion` and sets it to todo. */
export async function handOver(issue: Tracked): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(issue.key, { labels: ["legion"], priority: 1, status: "todo" });
}

/** The daemon admits the issue, and its architect claims it. */
export async function admit(issue: Tracked): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(issue.key, { status: "in_progress" }, DAEMON);
  await api.claimIssue(issue.key, agent(issue.key, "architect"));
}

/** The architect writes its design, with one decision block, into the spec; the block becomes an
 *  ask once the document service settles the edit. */
export async function writeDesign(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  await api.editArtifact(
    issue.spec,
    { ops: [{ after: "end", markdown: example.design, op: "insert" }] },
    agent(issue.key, "architect")
  );
  await expect.poll(async () => (await api.getIssue(issue.key)).open_asks.length).toBe(1);
}

/** The number of versions the spec has, to wait for the one an answer writes. */
export async function specVersions(issue: Tracked): Promise<number> {
  const api = await dispatchApi();
  return (await api.getArtifact(issue.spec)).versions.length;
}

/** Waits until the spec has more versions than `before`: an answer is written into its decision
 *  block as a new version, and an approval asked for before that version lands goes stale with it. */
export async function specVersionAfter(issue: Tracked, before: number): Promise<void> {
  const api = await dispatchApi();
  await expect
    .poll(async () => (await api.getArtifact(issue.spec)).versions.length)
    .toBeGreaterThan(before);
}

/** The human answers the decision block. */
export async function answerDecision(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  const [decision] = (await api.getIssue(issue.key)).open_asks;
  const versions = await specVersions(issue);
  await api.answerAsk(decision.id, { expected_edited_at: null, selected: [example.decision.pick] });
  await specVersionAfter(issue, versions);
}

/** The architect asks the human to approve the spec, with its summary; answers the ask's id. */
export async function requestSpecApproval(issue: Tracked, example: Example): Promise<string> {
  const api = await dispatchApi();
  const requested = await api.requestApproval(
    issue.spec,
    { summary: example.approvalSummary },
    agent(issue.key, "architect")
  );
  return requested.ask.id;
}

/** The human approves the spec. */
export async function approve(approval: string): Promise<void> {
  const api = await dispatchApi();
  await api.answerAsk(approval, { expected_edited_at: null, selected: ["Approve"] });
}

/** The implementer links the pull request it opened. */
export async function linkPullRequest(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(
    issue.key,
    { external_links: [{ url: example.pullRequest.url }] },
    agent(issue.key, "implementer")
  );
}

/** The statuses the daemon sets as the phases after implementation run. */
export const PHASE_STATUSES = ["testing", "needs_review", "retro"] as const;

/** The daemon sets one status. */
export async function setStatus(issue: Tracked, status: string): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(issue.key, { status }, DAEMON);
}

/** The first line of the READY message. */
export function readyLine(issue: Tracked, example: Example): string {
  const { number, url } = example.pullRequest;
  return `READY #${number} at ${example.head} (approved at ${example.head}) for ${issue.key} (${url})`;
}

/** The daemon posts the merger's READY on the issue. */
export async function postReady(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  await api.createMessage(
    issue.key,
    {
      body: [
        readyLine(issue, example),
        `Outcome: ${example.ready.outcome}`,
        `Not proven / risk: ${example.ready.risk}`,
      ].join("\n\n"),
    },
    DAEMON
  );
}

/** After the human's merge, the implementer records the production check and the architect signs
 *  off; the daemon closes the issue. */
export async function signOff(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  await api.createComment(issue.key, { body: example.production }, agent(issue.key, "implementer"));
  await api.createComment(issue.key, { body: example.signOff }, agent(issue.key, "architect"));
  await api.patchIssue(issue.key, { status: "done" }, DAEMON);
}

/** The whole journey, handover to sign-off, with no stop between the moves. */
export async function finish(issue: Tracked, example: Example): Promise<void> {
  await handOver(issue);
  await admit(issue);
  await writeDesign(issue, example);
  await answerDecision(issue, example);
  await approve(await requestSpecApproval(issue, example));
  await linkPullRequest(issue, example);
  for (const status of PHASE_STATUSES) await setStatus(issue, status);
  await postReady(issue, example);
  await signOff(issue, example);
}

const REPORT_SPEC =
  "## Summary\n\nLegion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. This issue is not work, so it carries no `legion` label and stays in icebox.\n";

/** The controller creates the project's `Legion daily report` issue, parks it in icebox through
 *  the daemon, and posts the day's report, naming each issue that closed today with its pull
 *  request. Answers the report issue. */
export async function postDailyReport(
  finished: readonly { readonly issue: Tracked; readonly example: Example }[]
): Promise<Tracked> {
  const api = await dispatchApi();
  const report = await api.createIssue(
    { project: PROJECT, spec: REPORT_SPEC, title: "Legion daily report" },
    CONTROLLER
  );
  await api.patchIssue(report.key, { status: "icebox" }, DAEMON);
  const lines = finished.map(
    ({ issue, example }) => `- ${issue.key} ${example.title}: merged in ${example.pullRequest.url}`
  );
  await api.createMessage(
    report.key,
    {
      body: [
        `Finished:\n${lines.join("\n")}`,
        "Closed without a change: none.",
        "Running: nothing is admitted, and nothing is waiting.",
        "Slots: 4 of 4 free. This turn's walk found no todo issue to take.",
      ].join("\n\n"),
    },
    CONTROLLER
  );
  return { key: report.key, spec: report.primary_artifact_id };
}
